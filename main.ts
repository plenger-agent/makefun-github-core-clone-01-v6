// Forge v4 — entrypoint. The forge itself is the vendored static Go
// binary beside this file (./forge); this launcher is only the harness
// lifecycle: it starts the binary, tells the env the port it bound, then
// waits until the env says the trial is over and exits.
//
//   1. MAKEFUN_ENV_URL names the env origin (http://store:8080 in a
//      trial). Without it there is nobody to register with, so exit 1.
//   2. Spawn ./forge with FORGE_PORT=0. The binary binds a kernel-chosen
//      port and prints "[forge] listening on port N". Never bind 8080:
//      the env owns it, and on the one-pod layout it shares this loopback.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the
//      address the request came from plus N as this entry's base URL.
//      The first register wins; a 409 means an earlier attempt already
//      registered and is not an error.
//   4. Poll GET $MAKEFUN_ENV_URL/done until it answers {"done": true}.
//      The env answers at once, so this is a poll, never a held request.
//   5. Stop the binary and exit 0. The harness stops the env after that,
//      and the env's export is what the scorer reads.

import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { once } from 'node:events';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const envBase = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (envBase === '') {
  console.error('[forge] MAKEFUN_ENV_URL is not set: no env to register with');
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30_000;
const FIRST_RETRY_MS = 250;
const MAX_RETRY_MS = 2_000;

function nap(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** The wait before retry `n` (0-based): doubling from FIRST_RETRY_MS, capped. */
function backoffMs(n: number): number {
  return Math.min(MAX_RETRY_MS, FIRST_RETRY_MS * 2 ** Math.min(n, 8));
}

/**
 * Report the bound port to the env, retrying through its startup lag.
 * 200 is the normal answer; 409 means a first register already won.
 * 400/405 are contract errors and stop the program; anything else is
 * retried, since the env may still be coming up.
 */
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const reply = await fetch(`${envBase}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (reply.status === 200 || reply.status === 409) return;
      if (reply.status === 400 || reply.status === 405) {
        throw new Error(`register answered HTTP ${reply.status}: ${await reply.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await nap(backoffMs(attempt));
  }
}

/**
 * Poll GET /done until the env answers {"done": true}; never return early.
 * While the env answers, poll on a fixed short beat; back off only while
 * the env is unreachable, when it is still starting.
 */
async function waitUntilDone(): Promise<void> {
  let unreachableStreak = 0;
  for (;;) {
    let reached = false;
    try {
      const reply = await fetch(`${envBase}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      reached = true;
      if (reply.ok) {
        const body = (await reply.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet: it is not done, so poll again.
    }
    unreachableStreak = reached ? 0 : unreachableStreak + 1;
    await nap(reached ? FIRST_RETRY_MS : backoffMs(unreachableStreak - 1));
  }
}

/** Start ./forge on an ephemeral port and read the port back from its first line. */
async function startForge(): Promise<{ child: ChildProcessWithoutNullStreams; port: number }> {
  const here = dirname(fileURLToPath(import.meta.url));
  const binary = join(here, 'forge');
  const child = spawn(binary, [], {
    env: { ...process.env, FORGE_PORT: '0' },
    stdio: ['ignore', 'pipe', 'inherit'],
  });
  const port = await new Promise<number>((resolve, reject) => {
    let buffer = '';
    const onData = (chunk: Buffer): void => {
      buffer += chunk.toString('utf8');
      const match = /listening on port (\d+)/.exec(buffer);
      if (match !== null) {
        child.stdout.off('data', onData);
        resolve(Number(match[1]));
      }
    };
    child.once('error', reject);
    child.once('exit', (code) => reject(new Error(`forge exited early with code ${code}`)));
    child.stdout.on('data', onData);
  });
  return { child, port };
}

/** Bind, register, wait for the env, shut down, exit 0. */
async function main(): Promise<void> {
  const { child, port } = await startForge();
  process.stdout.write(`[forge] core listening on port ${port}\n`);

  await register(port);
  process.stdout.write('[forge] registered; waiting for the env to say done\n');

  await waitUntilDone();
  child.kill('SIGTERM');
  await once(child, 'exit').catch(() => undefined);
  process.stdout.write('[forge] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
