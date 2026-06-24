package main

import (
	"strings"
	"testing"
)

func contains(links []string, want string) bool {
	for _, l := range links {
		if l == want {
			return true
		}
	}
	return false
}

func TestExtractProxyLinks_URLWithTrailingJunk(t *testing.T) {
	// Real channel text: links glued to markdown/emoji/Persian junk.
	text := `پروکسی (https://t.me/proxy?server=65.109.247.142&port=85&secret=FgMBAgABAAH8AxOG4kw63Q) ` +
		"tg://proxy?server=108.181.123.187&port=8888&secret=7gAA8A8Pd1VV____9QBuLmltZWRpYS5zdGVhbXBvd2VyZWQuY29t)[پروکسی\n" +
		"tg://proxy?server=195.254.165.216&port=9443&secret=FgMBAgABAAH8AwOG4kw63Q==\n" +
		"tg://proxy?server=87.248.149.6&port=8443&secret=eeNEgYdJvXrFGRMCIMJdCQ)[پروکسی"

	links := extractProxyLinksFromText(text)
	want := []string{
		"tg://proxy?server=65.109.247.142&port=85&secret=FgMBAgABAAH8AxOG4kw63Q",
		"tg://proxy?server=108.181.123.187&port=8888&secret=7gAA8A8Pd1VV____9QBuLmltZWRpYS5zdGVhbXBvd2VyZWQuY29t",
		"tg://proxy?server=195.254.165.216&port=9443&secret=FgMBAgABAAH8AwOG4kw63Q==",
		"tg://proxy?server=87.248.149.6&port=8443&secret=eeNEgYdJvXrFGRMCIMJdCQ",
	}
	for _, w := range want {
		if !contains(links, w) {
			t.Errorf("missing %q\n got: %v", w, links)
		}
	}
	// No junk should leak into any secret.
	for _, l := range links {
		if strings.ContainsAny(l, ")[*پروکسی ") {
			t.Errorf("junk leaked into link: %q", l)
		}
	}
}

func TestExtractProxyLinks_LabeledPlain(t *testing.T) {
	text := `[6/24/26 7:00 PM] Proxy MTProto: Server: 34.51.192.156
Port: 443
Secret: dd99c304c8857034be7df934a6e5a95032
@ProxyMTProto`
	links := extractProxyLinksFromText(text)
	want := "tg://proxy?server=34.51.192.156&port=443&secret=dd99c304c8857034be7df934a6e5a95032"
	if !contains(links, want) {
		t.Fatalf("labeled-plain not parsed: %v", links)
	}
}

func TestExtractProxyLinks_LabeledFancyUnicode(t *testing.T) {
	// Small-cap labels + a URL that carries the real host (label host is "unknown").
	text := "🚀ｎｅｗ ｐｒｏｘｙ🚀\n\n🌐sᴇʀᴠᴇʀ › unknown\n🔌 ᴘᴏʀᴛ ›  25565\n" +
		"🔓‎sᴇᴄʀᴇᴛ › ee104462821249bd7ac519130220c25d0963646e2e79656b74616e65742e636f6d\n\n" +
		"To SeT :  Connect Proxy (https://t.me/proxy?server=fresh.t-proxy.info.&port=25565&secret=ee104462821249bd7ac519130220c25d0963646e2e79656b74616e65742e636f6d)"
	links := extractProxyLinksFromText(text)
	want := "tg://proxy?server=fresh.t-proxy.info&port=25565&secret=ee104462821249bd7ac519130220c25d0963646e2e79656b74616e65742e636f6d"
	if !contains(links, want) {
		t.Fatalf("fancy-unicode/URL not parsed: %v", links)
	}
	// "unknown" host must never produce a link.
	for _, l := range links {
		if strings.Contains(l, "unknown") {
			t.Errorf("unknown host leaked: %q", l)
		}
	}
}

func TestExtractProxyLinks_DedupesAndDropsJunkPorts(t *testing.T) {
	text := `tg://proxy?server=1.2.3.4&port=443&secret=dd104462821249bd7ac519130220c25d09
tg://proxy?server=1.2.3.4&port=443&secret=dd104462821249bd7ac519130220c25d09
tg://proxy?server=9.9.9.9&port=443000&secret=eeNEgYdJvXrFGRMCIMJdCQ
tg://proxy?server=8.8.8.8&port=77777&secret=EERighJJvXrFGRMCIMJdCQ
tg://proxy?server=203.22.241.9&port=80&secret=8080`
	links := extractProxyLinksFromText(text)
	if len(links) != 1 {
		t.Fatalf("expected exactly 1 valid+deduped link, got %d: %v", len(links), links)
	}
	if links[0] != "tg://proxy?server=1.2.3.4&port=443&secret=dd104462821249bd7ac519130220c25d09" {
		t.Errorf("unexpected link: %v", links)
	}
}
