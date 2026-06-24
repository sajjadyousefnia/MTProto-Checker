package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-faster/errors"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ----------------------------------------------------------------------------
// Secure local storage for Telegram user credentials.
//
// The session (auth keys that grant access to the user account) and the
// api_id/api_hash are stored ONLY under the OS user-config directory
// (e.g. ~/.config/mtproto-checker on Linux), which lives entirely outside the
// project tree — it can never be committed to git. Files are created with 0600
// and the directory with 0700 so only the owning user can read them.
// ----------------------------------------------------------------------------

const (
	perChannelDefault   = 10  // proxies taken per channel when unspecified
	perChannelMax       = 100 // hard cap on proxies taken per channel
	historyScanMessages = 100 // recent messages scanned to fill the quota
	tgFetchTimeout      = 90 * time.Second
)

// tgConfigDir returns the per-user directory holding the session and app
// credentials, creating it with owner-only permissions if needed.
func tgConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		// Fall back to the home directory if UserConfigDir is unavailable.
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", errors.Wrap(err, "locate user config dir")
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "mtproto-checker")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errors.Wrap(err, "create config dir")
	}
	// Tighten perms in case the dir already existed with looser bits.
	_ = os.Chmod(dir, 0o700)
	return dir, nil
}

func sessionFilePath() (string, error) {
	dir, err := tgConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "session.json"), nil
}

func appConfigPath() (string, error) {
	dir, err := tgConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "app.json"), nil
}

type tgAppConfig struct {
	AppID   int    `json:"app_id"`
	AppHash string `json:"app_hash"`
}

// loadAppCreds resolves the api_id/api_hash to use. Priority:
//  1. TG_APP_ID + TG_APP_HASH environment variables
//  2. the locally saved app.json
//  3. the built-in public test credentials (fallback)
func loadAppCreds() (int, string) {
	if idStr := os.Getenv("TG_APP_ID"); idStr != "" {
		if hash := os.Getenv("TG_APP_HASH"); hash != "" {
			if id, err := strconv.Atoi(idStr); err == nil {
				return id, hash
			}
		}
	}
	if path, err := appConfigPath(); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			var cfg tgAppConfig
			if json.Unmarshal(data, &cfg) == nil && cfg.AppID != 0 && cfg.AppHash != "" {
				return cfg.AppID, cfg.AppHash
			}
		}
	}
	return testAppID, testAppHash
}

func saveAppCreds(id int, hash string) error {
	path, err := appConfigPath()
	if err != nil {
		return err
	}
	data, err := json.Marshal(tgAppConfig{AppID: id, AppHash: hash})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// ----------------------------------------------------------------------------
// MTProxy transport shared by check / login / fetch.
// ----------------------------------------------------------------------------

// buildMTProxyResolver creates a gotd DC resolver that tunnels through an
// MTProto proxy. Empty server falls back to the default (direct) resolver.
func buildMTProxyResolver(server string, port int, secret string) (dcs.Resolver, error) {
	if server == "" || port == 0 || secret == "" {
		return nil, errors.New("a working MTProto proxy (server, port, secret) is required")
	}
	addr := net.JoinHostPort(server, fmt.Sprintf("%d", port))
	decodedSecret, err := decodeSecret(secret)
	if err != nil {
		return nil, errors.Wrap(err, "decode secret")
	}
	resolver, err := dcs.MTProxy(addr, decodedSecret, dcs.MTProxyOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "create MTProxy resolver")
	}
	return resolver, nil
}

func newSessionStorage() (session.Storage, error) {
	path, err := sessionFilePath()
	if err != nil {
		return nil, err
	}
	return &session.FileStorage{Path: path}, nil
}

// ----------------------------------------------------------------------------
// Login manager: bridges the blocking gotd auth flow to stateless HTTP calls.
// ----------------------------------------------------------------------------

type ProxyCreds struct {
	Server string `json:"server"`
	Port   int    `json:"port"`
	Secret string `json:"secret"`
}

type loginManager struct {
	mu        sync.Mutex
	state     string // idle, starting, awaiting_captcha, awaiting_code, awaiting_password, authorized, failed
	errMsg    string
	phone     string
	sitekey   string // reCAPTCHA site key, set when state == awaiting_captcha
	codeCh    chan string
	pwdCh     chan string
	captchaCh chan string
	cancel    context.CancelFunc
}

var loginMgr = &loginManager{state: "idle"}

func (m *loginManager) snapshot() (state, errMsg, phone, sitekey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, m.errMsg, m.phone, m.sitekey
}

func (m *loginManager) setState(state string) {
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
}

func (m *loginManager) fail(err error) {
	m.mu.Lock()
	m.state = "failed"
	m.errMsg = err.Error()
	m.mu.Unlock()
}

// recaptchaSitekey extracts the reCAPTCHA site key from a Telegram
// RECAPTCHA_CHECK error. Telegram returns these as
// "RECAPTCHA_CHECK_<purpose>__<sitekey>" (e.g. the purpose is "signup" or
// "login"); the site key is everything after the final "__".
func recaptchaSitekey(err error) (string, bool) {
	rpcErr, ok := tgerr.As(err)
	if !ok {
		return "", false
	}
	idx := strings.Index(rpcErr.Message, "RECAPTCHA_CHECK")
	if idx < 0 {
		return "", false
	}
	parts := strings.SplitN(rpcErr.Message[idx:], "__", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// start kicks off a login. It returns immediately; progress is observed via
// snapshot() / the /tg/login/status endpoint.
func (m *loginManager) start(creds ProxyCreds, phone string, appID int, appHash string) error {
	m.mu.Lock()
	switch m.state {
	case "starting", "awaiting_captcha", "awaiting_code", "awaiting_password":
		m.mu.Unlock()
		return errors.New("a login is already in progress")
	}
	m.state = "starting"
	m.errMsg = ""
	m.phone = phone
	m.sitekey = ""
	m.codeCh = make(chan string, 1)
	m.pwdCh = make(chan string, 1)
	m.captchaCh = make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.mu.Unlock()

	resolver, err := buildMTProxyResolver(creds.Server, creds.Port, creds.Secret)
	if err != nil {
		cancel()
		m.fail(err)
		return err
	}
	storage, err := newSessionStorage()
	if err != nil {
		cancel()
		m.fail(err)
		return err
	}

	client := telegram.NewClient(appID, appHash, telegram.Options{
		Resolver:       resolver,
		SessionStorage: storage,
		Device:         telegram.DeviceTDesktopWindows(),
		NoUpdates:      true,
	})

	go func() {
		defer cancel()
		runErr := client.Run(ctx, func(ctx context.Context) error {
			return m.runAuth(ctx, client, phone, appID, appHash)
		})
		if runErr != nil {
			m.fail(runErr)
			return
		}
		m.setState("authorized")
	}()
	return nil
}

// runAuth drives the user authentication flow manually so that a reCAPTCHA
// challenge on send-code can be surfaced to the UI and retried with the solved
// token (via invokeWithReCaptcha). The rest of the flow (code, optional 2FA
// password) does not require a captcha.
func (m *loginManager) runAuth(ctx context.Context, client *telegram.Client, phone string, appID int, appHash string) error {
	authClient := client.Auth()

	sent, err := authClient.SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		sitekey, ok := recaptchaSitekey(err)
		if !ok {
			return err
		}
		// Surface the site key and wait for the browser-solved token.
		m.mu.Lock()
		m.sitekey = sitekey
		m.state = "awaiting_captcha"
		m.mu.Unlock()

		var token string
		select {
		case token = <-m.captchaCh:
		case <-ctx.Done():
			return ctx.Err()
		}

		// Retry the exact same send-code request, wrapped with the token.
		req := &tg.AuthSendCodeRequest{
			PhoneNumber: phone,
			APIID:       appID,
			APIHash:     appHash,
			Settings:    tg.CodeSettings{},
		}
		var box tg.AuthSentCodeBox
		if err := client.Invoke(ctx, &tg.InvokeWithReCaptchaRequest{Token: token, Query: req}, &box); err != nil {
			return err
		}
		sent = box.SentCode
	}

	sc, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return errors.Errorf("unexpected sent code type %T", sent)
	}
	codeHash := sc.PhoneCodeHash

	// Await the login code from the UI.
	m.setState("awaiting_code")
	var code string
	select {
	case code = <-m.codeCh:
	case <-ctx.Done():
		return ctx.Err()
	}

	_, err = authClient.SignIn(ctx, phone, code, codeHash)
	if errors.Is(err, auth.ErrPasswordAuthNeeded) {
		// 2FA: await the password from the UI.
		m.setState("awaiting_password")
		var pwd string
		select {
		case pwd = <-m.pwdCh:
		case <-ctx.Done():
			return ctx.Err()
		}
		if pwd == "" {
			return auth.ErrPasswordNotProvided
		}
		if _, err = authClient.Password(ctx, pwd); err != nil {
			return err
		}
		return nil
	}
	return err
}

func (m *loginManager) submitCaptcha(token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != "awaiting_captcha" {
		return errors.Errorf("not awaiting a captcha (state: %s)", m.state)
	}
	select {
	case m.captchaCh <- token:
		return nil
	default:
		return errors.New("captcha already submitted")
	}
}

func (m *loginManager) submitCode(code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != "awaiting_code" {
		return errors.Errorf("not awaiting a code (state: %s)", m.state)
	}
	select {
	case m.codeCh <- code:
		return nil
	default:
		return errors.New("code already submitted")
	}
}

func (m *loginManager) submitPassword(pwd string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != "awaiting_password" {
		return errors.Errorf("not awaiting a password (state: %s)", m.state)
	}
	select {
	case m.pwdCh <- pwd:
		return nil
	default:
		return errors.New("password already submitted")
	}
}

// isAuthorized opens a short-lived client to verify the stored session is
// still valid and authorized.
func isAuthorized(ctx context.Context, creds ProxyCreds) (bool, error) {
	resolver, err := buildMTProxyResolver(creds.Server, creds.Port, creds.Secret)
	if err != nil {
		return false, err
	}
	storage, err := newSessionStorage()
	if err != nil {
		return false, err
	}
	appID, appHash := loadAppCreds()
	client := telegram.NewClient(appID, appHash, telegram.Options{
		Resolver:       resolver,
		SessionStorage: storage,
		Device:         telegram.DeviceTDesktopWindows(),
		NoUpdates:      true,
	})

	var authorized bool
	runErr := client.Run(ctx, func(ctx context.Context) error {
		st, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		authorized = st.Authorized
		return nil
	})
	if runErr != nil {
		return false, runErr
	}
	return authorized, nil
}

// logout deletes the stored session so the account can no longer be accessed.
func logout() error {
	path, err := sessionFilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	loginMgr.mu.Lock()
	loginMgr.state = "idle"
	loginMgr.errMsg = ""
	loginMgr.phone = ""
	loginMgr.sitekey = ""
	loginMgr.mu.Unlock()
	return nil
}

// ----------------------------------------------------------------------------
// Fetching channel proxy links via the authenticated MTProto client.
// ----------------------------------------------------------------------------

// Channels post proxies in wildly different shapes. We recognise two:
//
//  1. A full link — tg://proxy?... or https://t.me/proxy?... — usually with
//     markdown/emoji/Persian text crammed right after the secret.
//  2. A label block — "Server: / Port: / Secret:" — sometimes dressed up with
//     small-capital unicode letters (sᴇʀᴠᴇʀ ›) and "Unknown" hostnames.
//
// Both are normalised to a canonical tg://proxy?server=&port=&secret= link so
// duplicates collapse regardless of the original formatting.
var (
	// proxyURLRe captures the three params of a proxy link. The secret group
	// stops at the first character that cannot belong to a hex/base64 secret,
	// which trims the trailing ")__", "**", ")پروکسی", emoji, etc.
	proxyURLRe = regexp.MustCompile(`(?i)(?:tg://|https?://t\.me/)proxy\?server=([^&\s"'<>]+)&port=(\d+)&secret=([A-Za-z0-9_+/=-]+)`)

	// proxyLabeledRe captures a "Server/Port/Secret" block (separator ":" or
	// "›", values on the same or following lines).
	proxyLabeledRe = regexp.MustCompile(`(?is)server\s*[:›]?\s*([A-Za-z0-9._-]+).*?port\s*[:›]?\s*(\d+).*?secret\s*[:›]?\s*([A-Za-z0-9_+/=-]+)`)

	// labelNormalizer maps the small-capital unicode letters channels use to
	// dress up the Server/Port/Secret labels back to ASCII, and drops
	// zero-width marks, so proxyLabeledRe can read them.
	labelNormalizer = strings.NewReplacer(
		"ᴀ", "a", "ʙ", "b", "ᴄ", "c", "ᴅ", "d", "ᴇ", "e",
		"ᴊ", "j", "ᴋ", "k", "ʟ", "l", "ᴍ", "m", "ɴ", "n",
		"ᴏ", "o", "ᴘ", "p", "ʀ", "r", "ᴛ", "t", "ᴜ", "u",
		"ᴠ", "v", "ɪ", "i",
		"\u200b", "", "\u200e", "", "\u200f", "", "\ufeff", "",
	)
)

// buildProxyLink validates the parts and renders a canonical proxy link, or
// reports false for junk (missing/"unknown" host, out-of-range port, stub
// secret).
func buildProxyLink(server, port, secret string) (string, bool) {
	server = strings.TrimRight(strings.TrimSpace(server), ".")
	if server == "" || strings.EqualFold(server, "unknown") {
		return "", false
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", false
	}
	if len(secret) < 8 {
		return "", false
	}
	return fmt.Sprintf("tg://proxy?server=%s&port=%d&secret=%s", server, p, secret), true
}

// collectProxyLinks appends canonical proxy links found in text (both URL and
// labeled forms) to out, skipping anything already in seen.
func collectProxyLinks(text string, seen map[string]struct{}, out []string) []string {
	add := func(server, port, secret string) []string {
		link, ok := buildProxyLink(server, port, secret)
		if !ok {
			return out
		}
		if _, dup := seen[link]; dup {
			return out
		}
		seen[link] = struct{}{}
		return append(out, link)
	}

	text = html.UnescapeString(text)
	for _, m := range proxyURLRe.FindAllStringSubmatch(text, -1) {
		out = add(m[1], m[2], m[3])
	}
	for _, m := range proxyLabeledRe.FindAllStringSubmatch(labelNormalizer.Replace(text), -1) {
		out = add(m[1], m[2], m[3])
	}
	return out
}

// extractProxyLinksFromText is the testable core: all unique proxy links in a
// single blob of text.
func extractProxyLinksFromText(text string) []string {
	return collectProxyLinks(text, make(map[string]struct{}), nil)
}

// extractProxyLinksFromMessage scans the raw message text and any text-URL
// entities (proxy links are often hidden behind formatted/button links where
// the visible text differs from the underlying URL).
func extractProxyLinksFromMessage(msg *tg.Message) []string {
	seen := make(map[string]struct{})
	out := collectProxyLinks(msg.Message, seen, nil)
	for _, e := range msg.Entities {
		if tu, ok := e.(*tg.MessageEntityTextURL); ok {
			out = collectProxyLinks(tu.URL, seen, out)
		}
	}
	return out
}

// fetchChannelViaTG returns up to maxProxies proxy links from a channel,
// taking the most recent ones first (Telegram history is newest-first). It
// scans message history until the quota is filled or the messages run out.
func fetchChannelViaTG(ctx context.Context, api *tg.Client, channel string, maxProxies int) ([]string, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: channel})
	if err != nil {
		return nil, errors.Wrap(err, "resolve username")
	}

	var peer tg.InputPeerClass
	for _, c := range resolved.Chats {
		if ch, ok := c.(*tg.Channel); ok {
			peer = &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
			break
		}
	}
	if peer == nil {
		// Could be a basic group or a user — fall back to chat if present.
		for _, c := range resolved.Chats {
			if chat, ok := c.(*tg.Chat); ok {
				peer = &tg.InputPeerChat{ChatID: chat.ID}
				break
			}
		}
	}
	if peer == nil {
		return nil, errors.New("not a channel/group, or no access")
	}

	hist, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  peer,
		Limit: historyScanMessages,
	})
	if err != nil {
		return nil, errors.Wrap(err, "get history")
	}
	modified, ok := hist.AsModified()
	if !ok {
		return nil, errors.New("unexpected history response")
	}

	var links []string
	seen := make(map[string]struct{})
	for _, m := range modified.GetMessages() { // newest first
		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		for _, l := range extractProxyLinksFromMessage(msg) {
			if _, dup := seen[l]; dup {
				continue
			}
			seen[l] = struct{}{}
			links = append(links, l)
			if maxProxies > 0 && len(links) >= maxProxies {
				return links, nil
			}
		}
	}
	return links, nil
}

type FetchTGRequest struct {
	Channels []string   `json:"channels"`
	Proxy    ProxyCreds `json:"proxy"`
	Limit    int        `json:"limit,omitempty"`
}

// fetchChannelsViaTelegram opens one authenticated client (through the given
// MTProto proxy) and pulls proxy links from each channel's recent history.
func fetchChannelsViaTelegram(ctx context.Context, req FetchTGRequest) (FetchChannelsResponse, error) {
	perChannel := req.Limit
	if perChannel <= 0 {
		perChannel = perChannelDefault
	}
	if perChannel > perChannelMax {
		perChannel = perChannelMax
	}

	// Normalize + dedupe channel names (reuses the existing helper).
	seenCh := make(map[string]struct{})
	var channels []string
	for _, c := range req.Channels {
		name := normalizeChannel(c)
		if name == "" {
			continue
		}
		if _, ok := seenCh[name]; ok {
			continue
		}
		seenCh[name] = struct{}{}
		channels = append(channels, name)
		if len(channels) >= maxChannels {
			break
		}
	}
	if len(channels) == 0 {
		return FetchChannelsResponse{}, errors.New("no valid channels provided")
	}

	resolver, err := buildMTProxyResolver(req.Proxy.Server, req.Proxy.Port, req.Proxy.Secret)
	if err != nil {
		return FetchChannelsResponse{}, err
	}
	storage, err := newSessionStorage()
	if err != nil {
		return FetchChannelsResponse{}, err
	}
	appID, appHash := loadAppCreds()
	client := telegram.NewClient(appID, appHash, telegram.Options{
		Resolver:       resolver,
		SessionStorage: storage,
		Device:         telegram.DeviceTDesktopWindows(),
		NoUpdates:      true,
	})

	runCtx, cancel := context.WithTimeout(ctx, tgFetchTimeout)
	defer cancel()

	var resp FetchChannelsResponse
	seenLink := make(map[string]struct{})

	runErr := client.Run(runCtx, func(ctx context.Context) error {
		st, err := client.Auth().Status(ctx)
		if err != nil {
			return errors.Wrap(err, "auth status")
		}
		if !st.Authorized {
			return errors.New("not logged in — complete Telegram login first")
		}

		api := client.API()
		for _, ch := range channels {
			// Telegram rate-limits resolve/getHistory; small gap between channels.
			links, err := fetchChannelViaTG(ctx, api, ch, perChannel)
			if err != nil {
				resp.Errors = append(resp.Errors, fmt.Sprintf("%s: %v", ch, err))
				continue
			}
			for _, l := range links {
				if _, ok := seenLink[l]; ok {
					continue
				}
				seenLink[l] = struct{}{}
				resp.Links = append(resp.Links, l)
			}
		}
		return nil
	})
	if runErr != nil {
		return FetchChannelsResponse{}, runErr
	}

	resp.Count = len(resp.Links)
	return resp, nil
}
