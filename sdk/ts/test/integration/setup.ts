// Builds the two programs the integration test runs, once per run:
// `silo` and internal/fakegrain, from this tree.
import { execFileSync } from 'node:child_process';
import { binDir, repoRoot } from './paths.js';

export default function setup() {
  for (const [out, pkg] of [
    ['silo', './cmd/silo'],
    ['fakegrain', './internal/fakegrain'],
  ]) {
    execFileSync('go', ['build', '-o', `${binDir}/${out}`, pkg], { cwd: repoRoot, stdio: 'inherit' });
  }
}
