import { FORGE_ATTACHMENT_HREF_PREFIX } from '../../../utils/forgeAttachments';
import { PATH_LINK_HREF_PREFIX } from '../../../utils/pathLinkExtension';

/** The link prefixes ChatMarkdown's renderer approves: `*` (every scheme
 * but the denied ones), its path links and forge attachments. Find's
 * projection classifies links with the same list. */
export const CHAT_LINK_PREFIXES: readonly string[] = ['*', PATH_LINK_HREF_PREFIX, FORGE_ATTACHMENT_HREF_PREFIX];
