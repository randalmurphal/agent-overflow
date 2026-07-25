/**
 * Auto-vendored during build. Do not edit manually.
 */

/**
 * HTML tag definitions following the WHATWG HTML Living Standard
 * https://html.spec.whatwg.org/
 *
 * §13.1.2 - Elements
 * §13.1.2.4 - Optional tags
 *
 * Optimized for fast tag lookup using first-character indexing.
 */

#ifndef TREE_SITTER_HTML_TAG_H_
#define TREE_SITTER_HTML_TAG_H_

#include "tree_sitter/array.h"
#include <stdbool.h>
#include <stdint.h>
#include <string.h>

/**
 * Tag types enumeration
 *
 * Organization:
 * 1. Void elements (§13.1.2) - cannot have content, no end tag
 * 2. Raw text elements (§13.1.2.1) - content is raw text
 * 3. Escapable raw text elements (§13.1.2.2) - content is escapable raw text
 * 4. Normal elements - ordered alphabetically
 * 5. Custom elements
 */
typedef enum {
  // =========================================================================
  // VOID ELEMENTS (§13.1.2)
  // These elements cannot have any contents and must not have an end tag.
  // Per the HTML Living Standard, these are the ONLY void elements:
  // =========================================================================
  AREA,
  BASE,
  BR,
  COL,
  EMBED,
  HR,
  IMG,
  INPUT,
  KEYGEN,
  LINK,
  META,
  PARAM,
  SOURCE,
  TRACK,
  WBR,
  END_OF_VOID_TAGS, // Sentinel for void element detection

  // =========================================================================
  // RAW TEXT ELEMENTS (§13.1.2.1)
  // Content is raw text - no character references or other elements parsed
  // =========================================================================
  SCRIPT,
  STYLE,
  END_OF_RAW_TEXT_TAGS, // Sentinel for raw text element detection

  // =========================================================================
  // ESCAPABLE RAW TEXT ELEMENTS (§13.1.2.2)
  // Content can have character references but no elements
  // =========================================================================
  TEXTAREA,
  TITLE,
  END_OF_ESCAPABLE_RAW_TEXT_TAGS, // Sentinel

  // =========================================================================
  // TEMPLATE ELEMENT
  // Special handling - contents are in a DocumentFragment
  // =========================================================================
  TEMPLATE,

  // =========================================================================
  // NORMAL ELEMENTS
  // Alphabetically ordered for easier maintenance
  // =========================================================================
  A,
  ABBR,
  ADDRESS,
  ARTICLE,
  ASIDE,
  AUDIO,
  B,
  BDI,
  BDO,
  BLOCKQUOTE,
  BODY,
  BUTTON,
  CANVAS,
  CAPTION,
  CITE,
  CODE,
  COLGROUP,
  DATA,
  DATALIST,
  DD,
  DEL,
  DETAILS,
  DFN,
  DIALOG,
  DIV,
  DL,
  DT,
  EM,
  FIELDSET,
  FIGCAPTION,
  FIGURE,
  FOOTER,
  FORM,
  H1,
  H2,
  H3,
  H4,
  H5,
  H6,
  HEAD,
  HEADER,
  HGROUP,
  HTML,
  I,
  IFRAME,
  INS,
  KBD,
  LABEL,
  LEGEND,
  LI,
  MAIN,
  MAP,
  MARK,
  MATH, // MathML integration point
  MENU,
  METER,
  NAV,
  NOSCRIPT,
  OBJECT,
  OL,
  OPTGROUP,
  OPTION,
  OUTPUT,
  P,
  PICTURE,
  PRE,
  PROGRESS,
  Q,
  RB,
  RP,
  RT,
  RTC,
  RUBY,
  S,
  SAMP,
  SEARCH, // New in HTML Living Standard
  SECTION,
  SELECT,
  SLOT,
  SMALL,
  SPAN,
  STRONG,
  SUB,
  SUMMARY,
  SUP,
  SVG, // SVG integration point
  TABLE,
  TBODY,
  TD,
  TFOOT,
  TH,
  THEAD,
  TIME,
  TR,
  U,
  UL,
  VAR,
  VIDEO,

  // =========================================================================
  // CUSTOM ELEMENTS
  // Any tag name with a hyphen, or unrecognized names
  // =========================================================================
  CUSTOM,

  END_, // Total count sentinel
} TagType;

typedef Array(char) String;

// Tag entry with pre-computed length for fast comparison
typedef struct {
  const char *tag_name;
  uint8_t length;
  TagType tag_type;
} TagMapEntry;

typedef struct {
  TagType type;
  String custom_tag_name;
} Tag;

// ============================================================================
// Tag lookup table - SORTED BY FIRST CHARACTER for indexed lookup
// ============================================================================

static const TagMapEntry TAG_TABLE[] = {
    // A
    {"A", 1, A},
    {"ABBR", 4, ABBR},
    {"ADDRESS", 7, ADDRESS},
    {"AREA", 4, AREA},
    {"ARTICLE", 7, ARTICLE},
    {"ASIDE", 5, ASIDE},
    {"AUDIO", 5, AUDIO},
    // B
    {"B", 1, B},
    {"BASE", 4, BASE},
    {"BDI", 3, BDI},
    {"BDO", 3, BDO},
    {"BLOCKQUOTE", 10, BLOCKQUOTE},
    {"BODY", 4, BODY},
    {"BR", 2, BR},
    {"BUTTON", 6, BUTTON},
    // C
    {"CANVAS", 6, CANVAS},
    {"CAPTION", 7, CAPTION},
    {"CITE", 4, CITE},
    {"CODE", 4, CODE},
    {"COL", 3, COL},
    {"COLGROUP", 8, COLGROUP},
    // D
    {"DATA", 4, DATA},
    {"DATALIST", 8, DATALIST},
    {"DD", 2, DD},
    {"DEL", 3, DEL},
    {"DETAILS", 7, DETAILS},
    {"DFN", 3, DFN},
    {"DIALOG", 6, DIALOG},
    {"DIV", 3, DIV},
    {"DL", 2, DL},
    {"DT", 2, DT},
    // E
    {"EM", 2, EM},
    {"EMBED", 5, EMBED},
    // F
    {"FIELDSET", 8, FIELDSET},
    {"FIGCAPTION", 10, FIGCAPTION},
    {"FIGURE", 6, FIGURE},
    {"FOOTER", 6, FOOTER},
    {"FORM", 4, FORM},
    // H
    {"H1", 2, H1},
    {"H2", 2, H2},
    {"H3", 2, H3},
    {"H4", 2, H4},
    {"H5", 2, H5},
    {"H6", 2, H6},
    {"HEAD", 4, HEAD},
    {"HEADER", 6, HEADER},
    {"HGROUP", 6, HGROUP},
    {"HR", 2, HR},
    {"HTML", 4, HTML},
    // I
    {"I", 1, I},
    {"IFRAME", 6, IFRAME},
    {"IMG", 3, IMG},
    {"INPUT", 5, INPUT},
    {"INS", 3, INS},
    // K
    {"KBD", 3, KBD},
    {"KEYGEN", 6, KEYGEN},
    // L
    {"LABEL", 5, LABEL},
    {"LEGEND", 6, LEGEND},
    {"LI", 2, LI},
    {"LINK", 4, LINK},
    // M
    {"MAIN", 4, MAIN},
    {"MAP", 3, MAP},
    {"MARK", 4, MARK},
    {"MATH", 4, MATH},
    {"MENU", 4, MENU},
    {"META", 4, META},
    {"METER", 5, METER},
    // N
    {"NAV", 3, NAV},
    {"NOSCRIPT", 8, NOSCRIPT},
    // O
    {"OBJECT", 6, OBJECT},
    {"OL", 2, OL},
    {"OPTGROUP", 8, OPTGROUP},
    {"OPTION", 6, OPTION},
    {"OUTPUT", 6, OUTPUT},
    // P
    {"P", 1, P},
    {"PARAM", 5, PARAM},
    {"PICTURE", 7, PICTURE},
    {"PRE", 3, PRE},
    {"PROGRESS", 8, PROGRESS},
    // Q
    {"Q", 1, Q},
    // R
    {"RB", 2, RB},
    {"RP", 2, RP},
    {"RT", 2, RT},
    {"RTC", 3, RTC},
    {"RUBY", 4, RUBY},
    // S
    {"S", 1, S},
    {"SAMP", 4, SAMP},
    {"SCRIPT", 6, SCRIPT},
    {"SEARCH", 6, SEARCH},
    {"SECTION", 7, SECTION},
    {"SELECT", 6, SELECT},
    {"SLOT", 4, SLOT},
    {"SMALL", 5, SMALL},
    {"SOURCE", 6, SOURCE},
    {"SPAN", 4, SPAN},
    {"STRONG", 6, STRONG},
    {"STYLE", 5, STYLE},
    {"SUB", 3, SUB},
    {"SUMMARY", 7, SUMMARY},
    {"SUP", 3, SUP},
    {"SVG", 3, SVG},
    // T
    {"TABLE", 5, TABLE},
    {"TBODY", 5, TBODY},
    {"TD", 2, TD},
    {"TEMPLATE", 8, TEMPLATE},
    {"TEXTAREA", 8, TEXTAREA},
    {"TFOOT", 5, TFOOT},
    {"TH", 2, TH},
    {"THEAD", 5, THEAD},
    {"TIME", 4, TIME},
    {"TITLE", 5, TITLE},
    {"TR", 2, TR},
    {"TRACK", 5, TRACK},
    // U
    {"U", 1, U},
    {"UL", 2, UL},
    // V
    {"VAR", 3, VAR},
    {"VIDEO", 5, VIDEO},
    // W
    {"WBR", 3, WBR},
};

#define TAG_TABLE_SIZE (sizeof(TAG_TABLE) / sizeof(TagMapEntry))

// ============================================================================
// First-character index table for O(1) bucket lookup
// Maps 'A'-'Z' (0-25) to {start, end} indices in TAG_TABLE
// ============================================================================

typedef struct {
  uint8_t start;
  uint8_t end; // exclusive
} CharBucket;

// Index: 0=A, 1=B, ... 25=Z
// Computed from TAG_TABLE entries (see counts in comments)
static const CharBucket CHAR_INDEX[26] = {
    {0, 7},     // A: A, ABBR, ADDRESS, AREA, ARTICLE, ASIDE, AUDIO (7)
    {7, 15},    // B: B, BASE, BDI, BDO, BLOCKQUOTE, BODY, BR, BUTTON (8)
    {15, 21},   // C: CANVAS, CAPTION, CITE, CODE, COL, COLGROUP (6)
    {21, 31},   // D: DATA..DT (10)
    {31, 33},   // E: EM, EMBED (2)
    {33, 38},   // F: FIELDSET..FORM (5)
    {38, 38},   // G: (none)
    {38, 49},   // H: H1..HTML (11)
    {49, 54},   // I: I..INS (5)
    {54, 54},   // J: (none)
    {54, 56},   // K: KBD, KEYGEN (2)
    {56, 60},   // L: LABEL..LINK (4)
    {60, 67},   // M: MAIN..METER (7)
    {67, 69},   // N: NAV, NOSCRIPT (2)
    {69, 74},   // O: OBJECT..OUTPUT (5)
    {74, 79},   // P: P..PROGRESS (5)
    {79, 80},   // Q: Q (1)
    {80, 85},   // R: RB..RUBY (5)
    {85, 101},  // S: S..SVG (16)
    {101, 113}, // T: TABLE..TRACK (12)
    {113, 115}, // U: U, UL (2)
    {115, 117}, // V: VAR, VIDEO (2)
    {117, 118}, // W: WBR (1)
    {118, 118}, // X: (none)
    {118, 118}, // Y: (none)
    {118, 118}, // Z: (none)
};

/**
 * Fast tag name lookup using first-character index
 * O(1) to find bucket, then O(k) where k is small (~5-10 tags per bucket)
 */
static inline TagType tag_type_for_name(const String *tag_name) {
  if (tag_name->size == 0 || tag_name->size > 10) {
    return CUSTOM;
  }

  char first = tag_name->contents[0];

  // Must be uppercase A-Z (scanner uppercases all tag names)
  if (first < 'A' || first > 'Z') {
    return CUSTOM;
  }

  // Quick reject: custom elements contain hyphens
  for (uint32_t i = 0; i < tag_name->size; i++) {
    if (tag_name->contents[i] == '-') {
      return CUSTOM;
    }
  }

  // O(1) bucket lookup
  const CharBucket *bucket = &CHAR_INDEX[first - 'A'];

  // Search only within the bucket
  for (uint8_t i = bucket->start; i < bucket->end; i++) {
    const TagMapEntry *entry = &TAG_TABLE[i];

    // Length check first (cheapest)
    if (entry->length != tag_name->size) {
      continue;
    }

    // Full comparison
    if (memcmp(tag_name->contents, entry->tag_name, tag_name->size) == 0) {
      return entry->tag_type;
    }
  }

  return CUSTOM;
}

/**
 * Elements that implicitly close a <p> element
 * Per §13.1.2.4 - Optional tags
 */
static const TagType TAG_TYPES_NOT_ALLOWED_IN_PARAGRAPHS[] = {
    ADDRESS,    ARTICLE, ASIDE,  BLOCKQUOTE, DETAILS, DIV,   DL,   FIELDSET,
    FIGCAPTION, FIGURE,  FOOTER, FORM,       H1,      H2,    H3,   H4,
    H5,         H6,      HEADER, HGROUP,     HR,      MAIN,  MENU, NAV,
    OL,         P,       PRE,    SEARCH,     SECTION, TABLE, UL,
};

#define P_CLOSING_TAGS_SIZE                                                    \
  (sizeof(TAG_TYPES_NOT_ALLOWED_IN_PARAGRAPHS) / sizeof(TagType))

static inline Tag tag_new(void) {
  Tag tag;
  tag.type = END_;
  tag.custom_tag_name = (String)array_new();
  return tag;
}

static inline Tag tag_for_name(String name) {
  Tag tag = tag_new();
  tag.type = tag_type_for_name(&name);
  if (tag.type == CUSTOM) {
    tag.custom_tag_name = name;
  } else {
    array_delete(&name);
  }
  return tag;
}

static inline void tag_free(Tag *tag) {
  if (tag->type == CUSTOM) {
    array_delete(&tag->custom_tag_name);
  }
}

static inline bool tag_is_void(const Tag *self) {
  return self->type < END_OF_VOID_TAGS;
}

static inline bool tag_is_raw_text(const Tag *self) {
  return self->type > END_OF_VOID_TAGS && self->type < END_OF_RAW_TEXT_TAGS;
}

static inline bool tag_is_escapable_raw_text(const Tag *self) {
  return self->type > END_OF_RAW_TEXT_TAGS &&
         self->type < END_OF_ESCAPABLE_RAW_TEXT_TAGS;
}

static inline bool tag_eq(const Tag *self, const Tag *other) {
  if (self->type != other->type)
    return false;
  if (self->type == CUSTOM) {
    if (self->custom_tag_name.size != other->custom_tag_name.size) {
      return false;
    }
    if (memcmp(self->custom_tag_name.contents, other->custom_tag_name.contents,
               self->custom_tag_name.size) != 0) {
      return false;
    }
  }
  return true;
}

/**
 * Determines if a parent element can contain a child element
 * Based on HTML content model rules (§4)
 */
static inline bool tag_can_contain(Tag *self, const Tag *other) {
  TagType child = other->type;

  switch (self->type) {
  // <li> closes when another <li> is seen
  case LI:
    return child != LI;

  // <dt> and <dd> close each other
  case DT:
  case DD:
    return child != DT && child != DD;

  // <p> has many elements that cause implicit closing
  case P:
    for (unsigned int i = 0; i < P_CLOSING_TAGS_SIZE; i++) {
      if (child == TAG_TYPES_NOT_ALLOWED_IN_PARAGRAPHS[i]) {
        return false;
      }
    }
    return true;

  // <colgroup> can only contain <col> and template
  case COLGROUP:
    return child == COL || child == TEMPLATE;

  // Ruby elements close each other
  case RB:
  case RT:
  case RP:
  case RTC:
    return child != RB && child != RT && child != RP && child != RTC;

  // <optgroup> closes when another <optgroup> is seen
  case OPTGROUP:
    return child != OPTGROUP;

  // <option> closes when another <option> or <optgroup> is seen
  case OPTION:
    return child != OPTION && child != OPTGROUP;

  // <tr> closes when another <tr> is seen
  case TR:
    return child != TR;

  // <td> and <th> close each other, and when <tr> is seen
  case TD:
  case TH:
    return child != TD && child != TH && child != TR;

  // <thead>, <tbody>, <tfoot> close each other
  case THEAD:
  case TBODY:
  case TFOOT:
    return child != THEAD && child != TBODY && child != TFOOT;

  // <caption> closes when table content starts
  case CAPTION:
    return child != THEAD && child != TBODY && child != TFOOT && child != TR &&
           child != COLGROUP && child != COL;

  // <head> closes when <body> or body-content is seen
  case HEAD:
    return child != BODY;

  default:
    return true;
  }
}

#endif // TREE_SITTER_HTML_TAG_H_
