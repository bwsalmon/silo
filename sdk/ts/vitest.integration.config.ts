// The SDK's integration test (test/integration): a real `silo hub
// serve` and a fake Grain, built from this tree by setup.ts, which
// needs Go. CI runs it in .grain/workflows/ui.yml's sdk job.
import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    environment: 'node',
    include: ['test/integration/**/*.integration.ts'],
    globalSetup: ['test/integration/setup.ts'],
    testTimeout: 30_000,
    hookTimeout: 120_000,
  },
});
