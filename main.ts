// Launcher for the github-core-clone Go forge.
//
// The forge itself is a statically linked Linux amd64 Go binary,
// ./forge, committed in this repository as vendored source (the
// competition allows a statically linked binary in place of an
// interpreted entrypoint). The trial harness runs this file as
// `node /bot/main.ts`; all this file does is spawn the binary in the
// same directory, forward stdio, and translate signals:
//
//   - SIGTERM/SIGINT are forwarded to the forge, which closes its
//     listener and exits 0 (a 143 exit reads as a crash to the harness).
//   - When the forge exits, this process exits with the same code.
//
// The forge binary does the real lifecycle itself: it binds an
// ephemeral port, POSTs it to $MAKEFUN_ENV_URL/register, polls
// $MAKEFUN_ENV_URL/done, and exits 0 when the trial is over.
// The Go sources beside this file (main.go, app.go, git.go, rest.go,
// graphql.go, mcp.go, transport.go, store.go, types.go, util.go) are
// the program the binary was built from.

import { spawn, type ChildProcess } from 'node:child_process';
import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, 'forge');

if (!existsSync(binary)) {
  process.stderr.write('[launcher] ./forge binary is missing from the repository\n');
  process.exit(1);
}

const child: ChildProcess = spawn(binary, [], {
  stdio: 'inherit',
  env: process.env,
});

let exiting = false;
const forward = (signal: NodeJS.Signals): void => {
  if (exiting) return;
  exiting = true;
  try {
    child.kill(signal);
  } catch {
    // The child is already gone; the exit handler below finishes us.
  }
  // If the child ignores the signal, do not hang the trial: force the
  // exit after 5 s, matching the Node entry's SIGTERM fallback.
  setTimeout(() => process.exit(0), 5_000).unref();
};
process.once('SIGTERM', () => forward('SIGTERM'));
process.once('SIGINT', () => forward('SIGINT'));

child.once('error', (err) => {
  process.stderr.write(`[launcher] failed to start ./forge: ${err.message}\n`);
  process.exit(1);
});
child.once('exit', (code, signal) => {
  if (signal !== null) {
    process.exit(1);
  }
  process.exit(code ?? 1);
});
