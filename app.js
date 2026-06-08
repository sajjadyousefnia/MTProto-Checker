import express from 'express';
import open from 'open';
import { TelegramClient, Api } from 'telegram';
import { StringSession } from 'telegram/sessions/index.js';
import path from 'path';
import { fileURLToPath } from 'url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

const app = express();
const PORT = 3000;

const API_ID = 6;
const API_HASH = 'eb06d4abfb49dc3eeb1aeb98ae0f581e';

app.use(express.static(path.join(__dirname, 'public'), { maxAge: '1d' }));
app.use(express.json({ limit: '50mb' }));
app.disable('x-powered-by');

app.post('/check', async (req, res) => {
    const { server, port, secret } = req.body;
    const TIMEOUT = 8000;

    const client = new TelegramClient(new StringSession(''), API_ID, API_HASH, {
        connectionRetries: 1,
        useWSS: false,
        proxy: { ip: server, port, secret, MTProxy: true, socksType: 5, timeout: 4 }
    });

    client.setLogLevel("none");

    const checkPromise = (async () => {
        const start = Date.now();
        try {
            await client.connect();
            await client.invoke(new Api.help.GetConfig());
            const ping = Date.now() - start;
            await client.disconnect();
            return ping;
        } catch (err) {
            try { await client.destroy(); } catch { }
            throw err;
        }
    })();

    const timeoutPromise = new Promise((_, reject) =>
        setTimeout(() => reject(new Error('TIMEOUT')), TIMEOUT)
    );

    try {
        const ping = await Promise.race([checkPromise, timeoutPromise]);
        res.json({ ok: true, ping });
    } catch {
        res.json({ ok: false });
    }
});

app.listen(PORT, async () => {
    console.log(`Server running at http://localhost:${PORT}`);
    try { await open(`http://localhost:${PORT}`); } catch (e) { }
});
