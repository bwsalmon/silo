// Browser E2E tests for Multi-User & Multi-Agent Shared Chats (/chats) and
// the Google Chat Space Link/Configuration UI.
import { expect, hubAdmin, test } from './fixtures';

test('shared chat session creation, participant invitation, two-user messaging, live streaming agent replies, and Google Chat link UI', async ({
  world,
  hub,
  pageAs,
  grain,
}) => {
  const invitedUser = `invited-${world.n}@e2e.test`;
  await hub.must(hubAdmin, 'PUT', `/v1/users/${encodeURIComponent(invitedUser)}`, { sys_admin: false });

  // Connect an interactive fakegrain agent for world.team.
  const node = await grain({
    owner: world.admin,
    repos: [world.repo],
    id: world.team,
    interactive: true,
  });

  const alicePage = await pageAs(world.admin);
  const invitedPage = await pageAs(invitedUser);

  // 1. Create a shared chat session from Alice's browser.
  await alicePage.goto('/chats');
  await alicePage.getByTestId('new-shared-chat-button').click();
  await alicePage.getByTestId('new-chat-title').fill(`Incident Room ${world.n}`);
  await alicePage.getByTestId('new-chat-deployment').fill(world.team);
  await alicePage.getByTestId('new-chat-repos').fill(world.repo);
  await alicePage.getByTestId('create-shared-chat-submit').click();

  await expect(alicePage).toHaveURL(/\/chats\/.+/);
  const chatUrl = new URL(alicePage.url());
  const chatPath = chatUrl.pathname;
  const chatId = chatPath.split('/').pop()!;

  // Wait for the connected fakegrain agent to accept the backing interactive session.
  await node.waitFor('shared chat backing session running on node', (s) => s.tasks.length >= 1);

  // 2. Invite another accepted user via the Participants & Context drawer.
  const drawer = alicePage.getByTestId('chat-context-drawer');
  await expect(drawer).toBeVisible();
  await drawer.getByTestId('invite-participant-email').fill(invitedUser);
  await drawer.getByTestId('invite-participant-btn').click();
  await expect(drawer.getByTestId(`participant-row-${invitedUser}`)).toBeVisible();

  // 3. Open the same shared chat in the invited user's browser context.
  await invitedPage.goto(chatPath);
  await expect(invitedPage.getByTestId('active-chat-title')).toHaveText(`Incident Room ${world.n}`);

  // 4. Send a human-to-human message from Alice and verify it streams live to the invited user's browser.
  await alicePage.getByTestId('chat-message-input').fill(`Hey @${invitedUser}, investigating the latency spike.`);
  await alicePage.getByTestId('send-chat-message-btn').click();

  await expect(alicePage.getByTestId('chat-transcript')).toContainText(
    `Hey @${invitedUser}, investigating the latency spike.`,
  );
  await expect(invitedPage.getByTestId('chat-transcript')).toContainText(
    `Hey @${invitedUser}, investigating the latency spike.`,
  );

  // 5. Send a message mentioning the agent from the invited user's browser and verify live streaming agent reply in both browsers.
  await invitedPage
    .getByTestId('chat-message-input')
    .fill(`@${world.team} please summarize the recent error logs.`);
  await invitedPage.getByTestId('send-chat-message-btn').click();

  await expect(alicePage.getByTestId('chat-transcript')).toContainText(
    `@${world.team} please summarize the recent error logs.`,
  );
  await expect(invitedPage.getByTestId('chat-transcript')).toContainText('Agent reply:');
  await expect(alicePage.getByTestId('chat-transcript')).toContainText('Agent reply:');

  // 6. Exercise the Google Chat space link/configuration UI.
  await alicePage.getByTestId('google-chat-badge').click();
  const gchatDialog = alicePage.getByTestId('google-chat-dialog');
  await expect(gchatDialog).toBeVisible();
  await expect(gchatDialog.getByTestId('google-chat-link-command')).toHaveText(`/grain link ${chatId}`);
  await expect(gchatDialog.getByTestId('google-chat-current-binding')).toContainText(
    'Not currently bound to a Google Chat space.',
  );

  await gchatDialog.getByTestId('copy-grain-link-btn').click();
  await expect(gchatDialog.getByTestId('copy-grain-link-btn')).toHaveText('Copied');

  await gchatDialog.getByTestId('google-chat-dialog-space').fill('spaces/AAAABBBB');
  await gchatDialog.getByTestId('google-chat-dialog-thread').fill('spaces/AAAABBBB/threads/t1');
  await gchatDialog.getByTestId('bind-google-chat-btn').click();

  await expect(gchatDialog.getByTestId('google-chat-current-binding')).toContainText(
    'Linked to space spaces/AAAABBBB · thread spaces/AAAABBBB/threads/t1',
  );
  await expect(alicePage.getByTestId('google-chat-badge')).toContainText(
    'Google Chat: spaces/AAAABBBB (spaces/AAAABBBB/threads/t1)',
  );

  // Unbind Google Chat and verify status resets.
  await gchatDialog.getByTestId('unbind-google-chat-btn').click();
  await expect(gchatDialog.getByTestId('google-chat-current-binding')).toContainText(
    'Not currently bound to a Google Chat space.',
  );
  await gchatDialog.getByTestId('close-google-chat-dialog-btn').click();
  await expect(alicePage.getByTestId('google-chat-badge')).toHaveText('Google Chat');
});
