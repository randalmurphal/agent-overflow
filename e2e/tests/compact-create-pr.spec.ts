// Create MR dialog on the compact layout against the fake `glab`: the chat
// header's thread actions menu opens the git actions menu ("Git
// actions…"), whose Create MR item opens the dialog. The phone has no
// command palette. Coverage is create-pr-flow.ts's header.
import { createPRFlow } from './create-pr-flow.js';

createPRFlow({
  forge: 'gitlab',
  openGitMenu: async (page) => {
    await page.getByTestId('chat-header-more').click();
    await page.getByRole('menuitem', { name: 'Git actions…' }).click();
  },
  openDialogElsewhere: null,
});
