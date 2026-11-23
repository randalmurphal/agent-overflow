import { FORGE_ATTACHMENT_HREF_PREFIX } from '../../../utils/forgeAttachments';
import { LOCAL_IMAGE_HREF_PREFIX, PATH_LINK_HREF_PREFIX } from '../../../utils/pathLinkExtension';
import { REPOSITORY_IMAGE_HREF_PREFIX } from '../../../utils/repositoryImageHref';

/** The link prefixes ChatMarkdown's renderer approves: `*` (every scheme
 * but the denied ones), its path links and forge attachments. Find's
 * projection classifies links with the same list, and the embedded HTML
 * sanitizer admits an `href` by it. */
export const CHAT_LINK_PREFIXES: readonly string[] = ['*', PATH_LINK_HREF_PREFIX, FORGE_ATTACHMENT_HREF_PREFIX];

/** The image prefixes ChatMarkdown's renderer and its HTML sanitizer
 * approve: `*`, its local images, forge attachments and repository
 * images, and `data:image/`.
 * `data:` is a denied LINK scheme (a document the webview would evaluate),
 * but an inline `data:image/…` src is just bytes for an <img>. */
export const CHAT_IMAGE_PREFIXES: readonly string[] = ['*', LOCAL_IMAGE_HREF_PREFIX, FORGE_ATTACHMENT_HREF_PREFIX, REPOSITORY_IMAGE_HREF_PREFIX, 'data:image/'];
