const translations = {
    fa: {
        title: "MTProto Pro Checker",
        subtitle: "تست اتصال",
        ready: "آماده برای شروع...",
        inputLabel: "📥 لیست ورودی (کثیف و نامرتب)",
        inputPlaceholder: "لینکهای پروکسی را اینجا وارد کنید (هر خط یک لینک)...",
        startBtn: "شروع بررسی",
        outputLabel: "🚀 لیست ۱۰۰٪ سالم",
        outputPlaceholder: "نتایج سالم اینجا نمایش داده میشوند...",
        copyBtn: "کپی کردن لیست سالم",
        processing: "در حال پردازش...",
        status: "وضعیت: {c} / {t} | سالم: {w}",
        toastCopied: "✅ کپی شد!",
        toastEmpty: "⚠️ لیست خالی است!",
        toastNoValid: "⛔ هیچ لینک معتبری یافت نشد!",
        toastNoWorking: "😔 هیچ پروکسی سالمی پیدا نشد.",
        toastFound: "🎉 {n} پروکسی سالم پیدا شد!",
        errorGeneric: "خطایی رخ داد. کنسول را چک کنید."
    },
    en: {
        title: "MTProto Pro Checker",
        subtitle: "Real connection test",
        ready: "Ready to start...",
        inputLabel: "📥 Input List (Mixed/Dirty)",
        inputPlaceholder: "Paste proxy links here (one per line)...",
        startBtn: "Start Check",
        outputLabel: "🚀 Working Proxies (100%)",
        outputPlaceholder: "Working proxies will appear here...",
        copyBtn: "Copy Working List",
        processing: "Processing...",
        status: "Status: {c} / {t} | Working: {w}",
        toastCopied: "✅ Copied to clipboard!",
        toastEmpty: "⚠️ List is empty!",
        toastNoValid: "⛔ No valid links found!",
        toastNoWorking: "😔 No working proxies found.",
        toastFound: "🎉 Found {n} working proxies!",
        errorGeneric: "An error occurred. Check console."
    }
};

let currentLang = localStorage.getItem('lang') || 'fa';

function setLanguage(lang) {
    currentLang = lang;
    localStorage.setItem('lang', lang);
    
    document.documentElement.dir = lang === 'fa' ? 'rtl' : 'ltr';
    document.documentElement.lang = lang;
    document.getElementById('langSelect').value = lang;

    document.querySelectorAll('[data-i18n]').forEach(el => {
        const key = el.getAttribute('data-i18n');
        if (translations[lang][key]) el.innerText = translations[lang][key];
    });

    document.getElementById('inputProxies').placeholder = translations[lang].inputPlaceholder;
    document.getElementById('outputProxies').placeholder = translations[lang].outputPlaceholder;
}

function changeLanguage(lang) {
    setLanguage(lang);
}

setLanguage(currentLang);

function log(msg, isError = false) {
    const c = document.getElementById('console');
    const line = document.createElement('div');
    line.innerText = `[${new Date().toLocaleTimeString()}] ${msg}`;
    if (isError) line.className = 'error-log';
    c.appendChild(line);
    c.scrollTop = c.scrollHeight;
}

window.onerror = function(message) {
    log(`CRITICAL ERROR: ${message}`, true);
};

let workingProxies = [];
let skippedCount = 0;

function parseLink(link) {
    try {
        let cleanLink = link.trim().replace('.&', '&');
        if(!cleanLink.includes('://')) return null;

        const urlObj = new URL(cleanLink);
        const params = new URLSearchParams(urlObj.search);
        
        const server = params.get('server');
        let port = parseInt(params.get('port'));
        const secret = params.get('secret');

        if (!server || !port || !secret || isNaN(port)) return null;
        if (port <= 0 || port > 65535) return null;

        if (secret.length > 170 || secret.includes('AAAAAAAAAAAAAAAAAAAA')) {
            skippedCount++;
            return null;
        }

        return { server, port, secret, original: cleanLink };
    } catch (e) { return null; }
}

async function startCheck() {
    try {
        const t = translations[currentLang];
        const input = document.getElementById('inputProxies').value;
        
        if (!input) return showToast(t.toastEmpty, true);

        const lines = input.split('\n');
        skippedCount = 0;
        const validLinks = lines.map(parseLink).filter(l => l !== null);

        if (validLinks.length === 0) {
            showToast(t.toastNoValid, true);
            log('Error: No valid links parsed', true);
            return;
        }

        log(`Parsed ${validLinks.length} valid links. Skipped ${skippedCount} bad links.`);

        workingProxies = [];
        document.getElementById('outputProxies').value = '';
        
        const startBtn = document.getElementById('startBtn');
        startBtn.disabled = true;
        startBtn.innerText = t.processing;

        let completed = 0;
        const total = validLinks.length;

        const checkOne = async (proxyData) => {
            try {
                const controller = new AbortController();
                const timeoutId = setTimeout(() => controller.abort(), 10000);

                const response = await fetch('/check', {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify(proxyData),
                    signal: controller.signal
                });
                clearTimeout(timeoutId);

                if (!response.ok) throw new Error('Server error');
                const result = await response.json();

                if (result.ok) {
                    workingProxies.push({ link: proxyData.original, ping: result.ping });
                    updateOutput();
                    log(`SUCCESS: ${proxyData.server} (${result.ping}ms)`);
                }
            } catch (err) {
                // Ignore
            } finally {
                completed++;
                updateUI(completed, total);
                if (completed === total) finish();
            }
        };

        const batchSize = 10;
        for (let i = 0; i < validLinks.length; i += batchSize) {
            const batch = validLinks.slice(i, i + batchSize);
            await Promise.all(batch.map(p => checkOne(p)));
        }
    } catch (e) {
        log(`MAIN ERROR: ${e.message}`, true);
        alert(translations[currentLang].errorGeneric);
        document.getElementById('startBtn').disabled = false;
    }
}

function updateUI(c, t) {
    const percent = (c / t) * 100;
    document.getElementById('progressBar').style.width = percent + '%';
    let statusText = translations[currentLang].status
        .replace('{c}', c)
        .replace('{t}', t)
        .replace('{w}', workingProxies.length);
    document.getElementById('statusText').innerText = statusText;
}

function updateOutput() {
    workingProxies.sort((a, b) => a.ping - b.ping);
    const text = workingProxies
        .map(p => `${p.link} # Ping: ${p.ping}ms`)
        .join('\n\n'); 
    document.getElementById('outputProxies').value = text;
}

function finish() {
    const t = translations[currentLang];
    const startBtn = document.getElementById('startBtn');
    startBtn.disabled = false;
    startBtn.innerText = t.startBtn;
    log('Process finished.');
    
    if (workingProxies.length > 0) {
        showToast(t.toastFound.replace('{n}', workingProxies.length));
    } else {
        showToast(t.toastNoWorking, true);
    }
}

function copyResults() {
    const t = translations[currentLang];
    const text = document.getElementById("outputProxies").value;
    if (!text) return showToast(t.toastEmpty, true);

    if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(() => {
            showToast(t.toastCopied);
        }).catch(() => fallbackCopy(text));
    } else {
        fallbackCopy(text);
    }
}

function fallbackCopy(text) {
    const t = translations[currentLang];
    const textArea = document.createElement("textarea");
    textArea.value = text;
    textArea.style.position = "fixed"; 
    document.body.appendChild(textArea);
    textArea.focus();
    textArea.select();
    try {
        document.execCommand('copy');
        showToast(t.toastCopied);
    } catch (err) {
        showToast('Error!', true);
    }
    document.body.removeChild(textArea);
}

function showToast(message, isError = false) {
    const toast = document.getElementById("toast");
    toast.innerText = message;
    toast.style.backgroundColor = isError ? "#ef4444" : "#10b981";
    toast.className = "toast show";
    setTimeout(() => { toast.className = toast.className.replace("show", ""); }, 3000);
}
