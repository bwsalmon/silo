// The SDK's unit tests, in Node (the browser's EventSource is faked
// where a test needs one). The integration test against a real hub is
// vitest.integration.config.ts's.
import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    environment: 'node',
    include: ['test/**/*.test.ts'],
  },
});
