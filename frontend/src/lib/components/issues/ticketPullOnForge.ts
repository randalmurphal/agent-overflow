// Opening a ticket's pull request on its forge: Work's row when the app
// cannot show it, its menu, and the bar over the review the app shows.

import { addToast } from '../../stores/toast.svelte';
import { handleExternalURL } from '../../utils/externalLinks';
import { pullNumberLabel } from '../pulls/pullRowView';

/** Opens `url`, the forge's page of pull request `number`; a link that is
 * no web address is said in a toast. */
export async function openPullOnForge(forge: string, number: number, url: string): Promise<void> {
  if (!(await handleExternalURL(url))) addToast('error', `Could not open ${pullNumberLabel(forge, number)}: its link ${url} is not a web address.`);
}
