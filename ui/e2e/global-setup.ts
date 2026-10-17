// Builds the two programs the suite runs, once per run: `silo`, with the
// bundle `npm run build` wrote to hub/ui/static/app embedded (a hub
// serving the placeholder fails every spec, so this refuses to go on
// without one), and internal/fakegrain.
import { execFileSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { binDir, repoRoot } from './paths';

export default function globalSetup() {
  if (!existsSync(`${repoRoot}/hub/ui/static/app/index.html`)) {
    throw new Error('no UI bundle in hub/ui/static/app: run `npm run build` first');
  }
  for (const [out, pkg] of [
    ['silo', './cmd/silo'],
    ['fakegrain', './internal/fakegrain'],
  ]) {
    execFileSync('go', ['build', '-o', `${binDir}/${out}`, pkg], { cwd: repoRoot, stdio: 'inherit' });
  }
}
