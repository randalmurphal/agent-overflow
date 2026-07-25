/**
 * External scanner for HTML grammar
 *
 * Follows the WHATWG HTML Living Standard:
 * https://html.spec.whatwg.org/
 *
 * Handles:
 * - Tag names (start, end, special elements)
 * - Raw text content (script, style)
 * - Escapable raw text content (textarea, title)
 * - Implicit end tags (§13.1.2.4)
 * - Comments (§13.6)
 * - Self-closing tag delimiter
 *
 * Performance optimizations:
 * - ASCII-only normalization for built-in tag matching
 * - Optimized delimiter matching with early exit
 * - Branch prediction hints
 */

#include "tag.h"
#include "tree_sitter/parser.h"

#include <stdbool.h>
#include <stdint.h>
#include <string.h>

// ============================================================================
// Performance macros
// ============================================================================

#if defined(__GNUC__) || defined(__clang__)
#define LIKELY(x) __builtin_expect(!!(x), 1)
#define UNLIKELY(x) __builtin_expect(!!(x), 0)
#define ALWAYS_INLINE __attribute__((always_inline)) inline
#else
#define LIKELY(x) (x)
#define UNLIKELY(x) (x)
#define ALWAYS_INLINE inline
#endif

// ============================================================================
// Token types - must match grammar.js externals order
// ============================================================================

enum TokenType {
  START_TAG_NAME,             // 0 - Normal element start tag
  RAW_TEXT_START_TAG_NAME,    // 1 - Raw/escapable raw text element (script, style, textarea, title)
  END_TAG_NAME,               // 2
  ERRONEOUS_END_TAG_NAME,     // 3
  SELF_CLOSING_TAG_DELIMITER, // 4
  IMPLICIT_END_TAG,           // 5
  RAW_TEXT,                   // 6
  COMMENT,                    // 7
  TEXT,                       // 8 - Text content including whitespace (§13.1.3)
};

// ============================================================================
// Scanner state
// ============================================================================

typedef struct {
  Array(Tag) tags;
} Scanner;

static ALWAYS_INLINE bool has_open_tag(Scanner *scanner) {
  return scanner->tags.size > 0;
}

static ALWAYS_INLINE Tag *current_tag(Scanner *scanner) {
  return array_back(&scanner->tags);
}

// ============================================================================
// ASCII-optimized character operations (no wchar overhead)
// ============================================================================

static ALWAYS_INLINE bool is_ascii_alpha(int32_t c) {
  return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z');
}

static ALWAYS_INLINE bool is_html_space(int32_t c) {
  // Accept HTML ASCII whitespace plus common Unicode/invisible separators so
  // hidden editor whitespace cannot be absorbed into tag names.
  return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r' ||
         c == '\v' || c == 0x0085 || c == 0x00A0 || c == 0x1680 ||
         (c >= 0x2000 && c <= 0x200B) ||
         c == 0x2028 || c == 0x2029 || c == 0x202F ||
         c == 0x205F || c == 0x3000 || c == 0xFEFF;
}

static ALWAYS_INLINE bool is_tag_name_char(int32_t c) {
  // Mirror the source parser: capture the whole tag token until a tag-name
  // terminator, then validate the resulting name later.
  return c != 0 && !is_html_space(c) && c != '/' && c != '>';
}

static ALWAYS_INLINE char to_ascii_upper(int32_t c) {
  return (c >= 'a' && c <= 'z') ? (char)(c - 32) : (char)c;
}

static ALWAYS_INLINE bool is_ascii_upper(int32_t c) {
  return c >= 'A' && c <= 'Z';
}

static ALWAYS_INLINE void push_utf8(String *string, int32_t c) {
  if (c <= 0x7F) {
    array_push(string, (char)c);
  } else if (c <= 0x7FF) {
    array_push(string, (char)(0xC0 | ((c >> 6) & 0x1F)));
    array_push(string, (char)(0x80 | (c & 0x3F)));
  } else if (c <= 0xFFFF) {
    array_push(string, (char)(0xE0 | ((c >> 12) & 0x0F)));
    array_push(string, (char)(0x80 | ((c >> 6) & 0x3F)));
    array_push(string, (char)(0x80 | (c & 0x3F)));
  } else {
    array_push(string, (char)(0xF0 | ((c >> 18) & 0x07)));
    array_push(string, (char)(0x80 | ((c >> 12) & 0x3F)));
    array_push(string, (char)(0x80 | ((c >> 6) & 0x3F)));
    array_push(string, (char)(0x80 | (c & 0x3F)));
  }
}

static ALWAYS_INLINE void push_tag_name_char(String *string, int32_t c) {
  push_utf8(string, c);
}

static ALWAYS_INLINE bool string_has_ascii_upper(const String *string) {
  for (uint32_t i = 0; i < string->size; i++) {
    unsigned char c = (unsigned char)string->contents[i];
    if (is_ascii_upper(c)) {
      return true;
    }
  }
  return false;
}

static ALWAYS_INLINE void normalize_tag_name(String *string) {
  for (uint32_t i = 0; i < string->size; i++) {
    unsigned char c = (unsigned char)string->contents[i];
    string->contents[i] = to_ascii_upper(c);
  }
}

static ALWAYS_INLINE Tag tag_for_htmlx_name(String tag_name) {
  if (string_has_ascii_upper(&tag_name)) {
    Tag tag = tag_new();
    tag.type = CUSTOM;
    tag.custom_tag_name = tag_name;
    return tag;
  }

  normalize_tag_name(&tag_name);
  return tag_for_name(tag_name);
}

// ============================================================================
// Lexer helpers
// ============================================================================

static ALWAYS_INLINE void advance(TSLexer *lexer) {
  lexer->advance(lexer, false);
}

static ALWAYS_INLINE void skip(TSLexer *lexer) {
  lexer->advance(lexer, true);
}

// ============================================================================
// Serialization (state persistence across parse calls)
// ============================================================================

static unsigned serialize(Scanner *scanner, char *buffer) {
  uint16_t tag_count = scanner->tags.size > UINT16_MAX
                           ? UINT16_MAX
                           : (uint16_t)scanner->tags.size;
  uint16_t serialized_tag_count = 0;

  unsigned size = sizeof(tag_count);
  memcpy(&buffer[size], &tag_count, sizeof(tag_count));
  size += sizeof(tag_count);

  for (; serialized_tag_count < tag_count; serialized_tag_count++) {
    Tag tag = scanner->tags.contents[serialized_tag_count];
    if (tag.type == CUSTOM) {
      unsigned name_length = tag.custom_tag_name.size;
      if (name_length > UINT8_MAX) {
        name_length = UINT8_MAX;
      }
      if (size + 2 + name_length >= TREE_SITTER_SERIALIZATION_BUFFER_SIZE) {
        break;
      }
      buffer[size++] = (char)tag.type;
      buffer[size++] = (char)name_length;
      memcpy(&buffer[size], tag.custom_tag_name.contents, name_length);
      size += name_length;
    } else {
      if (size + 1 >= TREE_SITTER_SERIALIZATION_BUFFER_SIZE) {
        break;
      }
      buffer[size++] = (char)tag.type;
    }
  }

  memcpy(&buffer[0], &serialized_tag_count, sizeof(serialized_tag_count));
  return size;
}

static void deserialize(Scanner *scanner, const char *buffer, unsigned length) {
  // Free existing tags
  for (unsigned i = 0; i < scanner->tags.size; i++) {
    tag_free(&scanner->tags.contents[i]);
  }
  array_clear(&scanner->tags);

  if (UNLIKELY(length == 0)) {
    return;
  }

  unsigned size = 0;
  uint16_t tag_count = 0;
  uint16_t serialized_tag_count = 0;

  memcpy(&serialized_tag_count, &buffer[size], sizeof(serialized_tag_count));
  size += sizeof(serialized_tag_count);

  memcpy(&tag_count, &buffer[size], sizeof(tag_count));
  size += sizeof(tag_count);

  array_reserve(&scanner->tags, tag_count);

  for (unsigned iter = 0; iter < serialized_tag_count; iter++) {
    Tag tag = tag_new();
    tag.type = (TagType)buffer[size++];
    if (tag.type == CUSTOM) {
      uint16_t name_length = (uint8_t)buffer[size++];
      array_reserve(&tag.custom_tag_name, name_length);
      tag.custom_tag_name.size = name_length;
      memcpy(tag.custom_tag_name.contents, &buffer[size], name_length);
      size += name_length;
    }
    array_push(&scanner->tags, tag);
  }

  // Add zero tags if we didn't read enough (buffer overflow protection)
  for (unsigned iter = serialized_tag_count; iter < tag_count; iter++) {
    array_push(&scanner->tags, tag_new());
  }
}

// ============================================================================
// Tag name scanning
// ============================================================================

/**
 * Scan tag name - returns String (caller owns)
 */
static String scan_tag_name(TSLexer *lexer) {
  String tag_name = array_new();
  while (is_tag_name_char(lexer->lookahead)) {
    push_tag_name_char(&tag_name, lexer->lookahead);
    advance(lexer);
  }
  return tag_name;
}

// ============================================================================
// Comment scanning
// ============================================================================

/**
 * Scan HTML comment
 * Per §13.6 - Comments start with <!-- and end with -->
 */
static bool scan_comment(TSLexer *lexer) {
  // Already consumed '<!'
  if (UNLIKELY(lexer->lookahead != '-')) {
    return false;
  }
  advance(lexer);

  if (UNLIKELY(lexer->lookahead != '-')) {
    return false;
  }
  advance(lexer);

  // Scan until we find -->
  unsigned dashes = 0;
  while (lexer->lookahead != 0) {
    int32_t c = lexer->lookahead;
    advance(lexer);

    if (c == '-') {
      dashes++;
    } else if (c == '>' && dashes >= 2) {
      lexer->result_symbol = COMMENT;
      lexer->mark_end(lexer);
      return true;
    } else {
      dashes = 0;
    }
  }

  return false;
}

// ============================================================================
// Raw text content scanning
// ============================================================================

// Pre-computed raw text end delimiters (uppercase for case-insensitive
// matching)
static const struct {
  TagType type;
  const char *delimiter;
  uint8_t length;
} RAW_TEXT_DELIMITERS[] = {
    {SCRIPT, "</SCRIPT", 8},
    {STYLE, "</STYLE", 7},
    {TEXTAREA, "</TEXTAREA", 10},
    {TITLE, "</TITLE", 7},
};

#define RAW_TEXT_DELIMITER_COUNT 4

/**
 * Scan raw text content for script, style, textarea, title elements
 * Per §13.1.2.1 and §13.1.2.2
 *
 * Optimized: uses pre-computed delimiter info, avoids strlen in hot path
 */
static bool scan_raw_text(Scanner *scanner, TSLexer *lexer) {
  if (UNLIKELY(!has_open_tag(scanner))) {
    return false;
  }

  TagType tag_type = current_tag(scanner)->type;

  // Find delimiter info
  const char *delimiter = NULL;
  unsigned delimiter_len = 0;

  for (int i = 0; i < RAW_TEXT_DELIMITER_COUNT; i++) {
    if (RAW_TEXT_DELIMITERS[i].type == tag_type) {
      delimiter = RAW_TEXT_DELIMITERS[i].delimiter;
      delimiter_len = RAW_TEXT_DELIMITERS[i].length;
      break;
    }
  }

  if (UNLIKELY(delimiter == NULL)) {
    return false;
  }

  lexer->mark_end(lexer);

  unsigned match_index = 0;

  while (lexer->lookahead != 0) {
    char upper = to_ascii_upper(lexer->lookahead);

    if (upper == delimiter[match_index]) {
      match_index++;
      if (match_index == delimiter_len) {
        // Found end delimiter - don't consume it
        break;
      }
      advance(lexer);
    } else {
      // Reset matching, mark position as content end
      match_index = 0;
      advance(lexer);
      lexer->mark_end(lexer);
    }
  }

  lexer->result_symbol = RAW_TEXT;
  return true;
}

// ============================================================================
// Tag stack management
// ============================================================================

static ALWAYS_INLINE void pop_tag(Scanner *scanner) {
  Tag popped_tag = array_pop(&scanner->tags);
  tag_free(&popped_tag);
}

// ============================================================================
// Implicit end tag scanning
// ============================================================================

/**
 * Scan for implicit end tags
 * Per §13.1.2.4 - Optional tags
 */
static bool scan_implicit_end_tag(Scanner *scanner, TSLexer *lexer) {
  Tag *parent = has_open_tag(scanner) ? current_tag(scanner) : NULL;

  bool is_closing_tag = false;
  if (lexer->lookahead == '/') {
    is_closing_tag = true;
    advance(lexer);
  } else {
    // Void elements implicitly close themselves
    if (parent && tag_is_void(parent)) {
      pop_tag(scanner);
      lexer->result_symbol = IMPLICIT_END_TAG;
      return true;
    }
  }

  String tag_name = scan_tag_name(lexer);

  if (tag_name.size == 0 && !lexer->eof(lexer)) {
    array_delete(&tag_name);
    return false;
  }

  Tag next_tag = tag_for_htmlx_name(tag_name);

  if (is_closing_tag) {
    // Check if tag correctly closes the topmost element
    if (has_open_tag(scanner) && tag_eq(current_tag(scanner), &next_tag)) {
      tag_free(&next_tag);
      return false;
    }

    // Search stack for matching tag - emit implicit end tags
    for (unsigned i = scanner->tags.size; i > 0; i--) {
      if (tag_eq(&scanner->tags.contents[i - 1], &next_tag)) {
        pop_tag(scanner);
        lexer->result_symbol = IMPLICIT_END_TAG;
        tag_free(&next_tag);
        return true;
      }
    }
  } else if (parent != NULL) {
    // Check content model - does parent allow this child?
    bool should_close = !tag_can_contain(parent, &next_tag);

    // Also close html/head/body at EOF
    if (!should_close && lexer->eof(lexer)) {
      TagType pt = parent->type;
      should_close = (pt == HTML || pt == HEAD || pt == BODY);
    }

    if (should_close) {
      pop_tag(scanner);
      lexer->result_symbol = IMPLICIT_END_TAG;
      tag_free(&next_tag);
      return true;
    }
  }

  tag_free(&next_tag);
  return false;
}

// ============================================================================
// Start tag scanning
// ============================================================================

static bool scan_start_tag_name(Scanner *scanner, TSLexer *lexer) {
  String tag_name = scan_tag_name(lexer);

  if (UNLIKELY(tag_name.size == 0)) {
    array_delete(&tag_name);
    return false;
  }

  Tag tag = tag_for_htmlx_name(tag_name);
  array_push(&scanner->tags, tag);

  // Determine token type: raw text elements vs normal elements
  switch (tag.type) {
  case SCRIPT:
  case STYLE:
  case TEXTAREA:
  case TITLE:
    lexer->result_symbol = RAW_TEXT_START_TAG_NAME;
    break;
  default:
    lexer->result_symbol = START_TAG_NAME;
    break;
  }

  return true;
}

// ============================================================================
// End tag scanning
// ============================================================================

static bool scan_end_tag_name(Scanner *scanner, TSLexer *lexer) {
  String tag_name = scan_tag_name(lexer);

  if (UNLIKELY(tag_name.size == 0)) {
    array_delete(&tag_name);
    return false;
  }

  Tag tag = tag_for_htmlx_name(tag_name);

  // Check if this closes the current element
  if (has_open_tag(scanner) && tag_eq(current_tag(scanner), &tag)) {
    pop_tag(scanner);
    lexer->result_symbol = END_TAG_NAME;
  } else {
    lexer->result_symbol = ERRONEOUS_END_TAG_NAME;
  }

  tag_free(&tag);
  return true;
}

// ============================================================================
// Self-closing tag delimiter
// ============================================================================

static bool scan_self_closing_tag_delimiter(Scanner *scanner, TSLexer *lexer) {
  advance(lexer); // consume '/'

  if (LIKELY(lexer->lookahead == '>')) {
    advance(lexer);
    if (has_open_tag(scanner)) {
      pop_tag(scanner);
      lexer->result_symbol = SELF_CLOSING_TAG_DELIMITER;
    }
    return true;
  }

  return false;
}

// ============================================================================
// Text content scanning
// ============================================================================

/**
 * Scan text content per §13.1.3
 *
 * Text is allowed inside elements and may contain any characters except:
 * - '<' (starts a tag)
 * - '&' (starts a character reference)
 *
 * Whitespace is significant and captured as part of the text node.
 */
static bool scan_text(TSLexer *lexer) {
  bool has_content = false;

  while (lexer->lookahead != 0) {
    int32_t c = lexer->lookahead;

    // Stop at tag start or character reference
    if (c == '<' || c == '&') {
      break;
    }

    advance(lexer);
    has_content = true;
  }

  if (LIKELY(has_content)) {
    lexer->mark_end(lexer);
    lexer->result_symbol = TEXT;
    return true;
  }

  return false;
}

static bool scan_void_implicit_end_tag(Scanner *scanner, TSLexer *lexer,
                                       const bool *valid_symbols) {
  if (!valid_symbols[IMPLICIT_END_TAG] || !has_open_tag(scanner)) {
    return false;
  }

  Tag *parent = current_tag(scanner);
  if (!tag_is_void(parent)) {
    return false;
  }

  lexer->mark_end(lexer);
  pop_tag(scanner);
  lexer->result_symbol = IMPLICIT_END_TAG;
  return true;
}

// ============================================================================
// Main scan function
// ============================================================================

static bool scan(Scanner *scanner, TSLexer *lexer, const bool *valid_symbols) {
  // Priority 1: Raw text mode - for script, style, textarea, title content
  if (valid_symbols[RAW_TEXT] && !valid_symbols[START_TAG_NAME] &&
      !valid_symbols[END_TAG_NAME]) {
    return scan_raw_text(scanner, lexer);
  }

  if (scan_void_implicit_end_tag(scanner, lexer, valid_symbols)) {
    return true;
  }

  // Priority 2: Text content - capture before whitespace is skipped
  // Text includes whitespace per §13.1.3
  if (valid_symbols[TEXT]) {
    if (scan_text(lexer)) {
      return true;
    }
  }

  // Skip whitespace (only when not in text content context)
  while (is_html_space(lexer->lookahead)) {
    skip(lexer);
  }

  int32_t lookahead = lexer->lookahead;

  // Priority 3: Check for tag/comment start
  if (lookahead == '<') {
    lexer->mark_end(lexer);
    advance(lexer);

    if (lexer->lookahead == '!') {
      advance(lexer);
      return scan_comment(lexer);
    }

    if (valid_symbols[IMPLICIT_END_TAG]) {
      return scan_implicit_end_tag(scanner, lexer);
    }

    return false;
  }

  // Priority 4: EOF handling
  if (lookahead == 0) {
    if (valid_symbols[IMPLICIT_END_TAG]) {
      return scan_implicit_end_tag(scanner, lexer);
    }
    return false;
  }

  // Priority 5: Self-closing tag delimiter
  if (lookahead == '/' && valid_symbols[SELF_CLOSING_TAG_DELIMITER]) {
    return scan_self_closing_tag_delimiter(scanner, lexer);
  }

  // Priority 6: Tag names (after < or </)
  if ((valid_symbols[START_TAG_NAME] || valid_symbols[END_TAG_NAME]) &&
      !valid_symbols[RAW_TEXT]) {
    if (valid_symbols[START_TAG_NAME]) {
      return scan_start_tag_name(scanner, lexer);
    } else {
      return scan_end_tag_name(scanner, lexer);
    }
  }

  return false;
}

// ============================================================================
// Tree-sitter external scanner interface
// ============================================================================

void *tree_sitter_html_external_scanner_create(void) {
  Scanner *scanner = (Scanner *)ts_calloc(1, sizeof(Scanner));
  return scanner;
}

bool tree_sitter_html_external_scanner_scan(void *payload, TSLexer *lexer,
                                            const bool *valid_symbols) {
  Scanner *scanner = (Scanner *)payload;
  return scan(scanner, lexer, valid_symbols);
}

unsigned tree_sitter_html_external_scanner_serialize(void *payload,
                                                     char *buffer) {
  Scanner *scanner = (Scanner *)payload;
  return serialize(scanner, buffer);
}

void tree_sitter_html_external_scanner_deserialize(void *payload,
                                                   const char *buffer,
                                                   unsigned length) {
  Scanner *scanner = (Scanner *)payload;
  deserialize(scanner, buffer, length);
}

void tree_sitter_html_external_scanner_destroy(void *payload) {
  Scanner *scanner = (Scanner *)payload;
  for (unsigned i = 0; i < scanner->tags.size; i++) {
    tag_free(&scanner->tags.contents[i]);
  }
  array_delete(&scanner->tags);
  ts_free(scanner);
}
