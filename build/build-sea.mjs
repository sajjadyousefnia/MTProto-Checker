// build pipeline for Windows SEA executable
import { execSync } from 'child_process';
import { copyFileSync, unlinkSync, existsSync, mkdirSync } from 'fs';
import { join, dirname } from 'path';
import { fileURLToPath } from 'url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const root = join(__dirname, '..');
const dist = join(root, 'dist');

function run(cmd, label) {
    console.log(`\n==> ${label}`);
    execSync(cmd, { cwd: root, stdio: 'inherit' });
}

if (!existsSync(dist)) mkdirSync(dist, { recursive: true });

// 1. Generate embedded assets
run('node build/generate-assets.mjs', 'Generate embedded assets');

// 2. Bundle with esbuild
run('npx esbuild build/sea-entry.mjs --bundle --platform=node --outfile=dist/sea-bundle.cjs --format=cjs', 'Bundle with esbuild');

// 3. Create SEA blob
run('node --experimental-sea-config build/sea-config.json', 'Create SEA blob');

// 4. Copy node.exe
const nodeSrc = process.execPath;
const exeDest = join(dist, 'MTProto-Checker.exe');
copyFileSync(nodeSrc, exeDest);
console.log(`\n==> Copied node.exe → ${exeDest}`);

// 5. Remove digital signature (Windows) — best effort
try {
    execSync(`powershell -Command "Remove-Item -LiteralPath '${exeDest}' -Force -Stream 'Zone.Identifier' 2>`+"$null", { stdio: 'ignore' });
} catch { }

// 6. Inject SEA blob
run(`npx postject "${exeDest}" NODE_SEA_BLOB "${join(dist, 'sea-prep.blob')}" --sentinel-fuse NODE_SEA_FUSE_fce680ab2cc467b6e072b8b5df1996b2`, 'Inject SEA blob');

// 7. Clean up intermediate files
for (const f of ['sea-bundle.cjs', 'sea-prep.blob']) {
    const p = join(dist, f);
    if (existsSync(p)) unlinkSync(p);
}

console.log(`\n✓ Build complete: ${exeDest}`);
