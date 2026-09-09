# Frontend bundle archive

This package turns the embedded SPA into a content-addressed manifest and a ZIP
containing exactly the manifest files. Transport owns HTTP and authentication;
mobile clients own installation. See the bundle-sync section of
[remote access](../../docs/specs/remote-access.md).

The bundle ID is derived from canonical manifest content and file bytes, not a
release version or build timestamp. Sort paths, normalize separators, and keep
metadata deterministic so identical trees produce identical IDs and archives
on every platform.

Refuse an empty tree, unsafe paths, traversal, absolute paths, separator aliases,
and content that changes between manifesting and archiving. Every archive entry
must correspond exactly to one manifest entry with its declared size and digest.
Do not include host filesystem metadata.

Stream file hashing. The archive is built lazily and retained as immutable bytes
for the process lifetime, so do not mutate the returned slice or add unrelated
files to the served tree. Native shell compatibility and release ordering are
separate from bundle identity.

The filesystem and embedded-FS implementations follow the same canonical rules
and share golden tests. Add fixtures for ordering, empty trees, unsafe names,
changed content, size limits, and byte-identical output.
