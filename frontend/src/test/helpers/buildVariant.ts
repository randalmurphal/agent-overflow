// Stage the page the `noremote` build serves: the backend stamps
// `<meta name="ao-remote-access" content="off">` into index.html. setup.ts
// restores the standard page after every test.

import { __resetBuildVariantForTest } from '../../lib/transport/buildVariant';

const SELECTOR = 'meta[name="ao-remote-access"]';

export function stageNoRemoteBuild(): void {
  const meta = document.createElement('meta');
  meta.name = 'ao-remote-access';
  meta.content = 'off';
  document.head.append(meta);
  __resetBuildVariantForTest();
}

export function resetBuildVariantPage(): void {
  for (const meta of document.querySelectorAll(SELECTOR)) meta.remove();
  __resetBuildVariantForTest();
}
