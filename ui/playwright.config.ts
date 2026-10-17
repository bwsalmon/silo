// The hub UI's browser suite (e2e/): a real Chromium, driven by
// Playwright, against a real `silo hub serve` with this tree's bundle
// embedded, and scripted fake Grains (internal/fakegrain) connected to
// it over the real rendezvous. e2e/fixtures.ts says how each piece is
// started; the specs only ever write paths.
//
// Each worker runs a hub of its own, on a port the kernel picks, over a
// SQLite file of its own (or the database SILO_E2E_DATABASE names, for
// a run against PostgreSQL), and each test makes its own team, users,
// repository and nodes with names no other test uses: so the files run
// in parallel and the tests in a file in any order.
import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './e2e',
  testMatch: /.*\.spec\.ts/,
  globalSetup: './e2e/global-setup.ts',
  fullyParallel: true,
  workers: process.env.CI ? 3 : undefined,
  retries: 0,
  timeout: 60_000,
  expect: { timeout: 15_000 },
  // The list on the console; on failure, a trace and a screenshot in
  // test-results, which CI keeps as an artifact.
  reporter: 'list',
  outputDir: 'test-results',
  use: {
    browserName: 'chromium',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
});
