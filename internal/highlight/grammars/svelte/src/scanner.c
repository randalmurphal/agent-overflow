#define tree_sitter_htmlx_external_scanner_create      htmlx_create
#define tree_sitter_htmlx_external_scanner_destroy     htmlx_destroy
#define tree_sitter_htmlx_external_scanner_scan        htmlx_scanner_scan
#define tree_sitter_htmlx_external_scanner_serialize   htmlx_serialize
#define tree_sitter_htmlx_external_scanner_deserialize htmlx_deserialize

// Vendored HTMLX scanner (committed in this crate for portability).
#include "htmlx/scanner.c"

// Svelte external token indices (after HTMLX's 26 tokens: 0-25)
// HTML tokens (0-8) + HTMLX tokens (9-25, includes UNTERMINATED_TAG_END_OPEN at 25)
enum {
    ITERATOR_EXPRESSION_JS = 26,
    ITERATOR_EXPRESSION_TS,
    BINDING_PATTERN_JS,
    BINDING_PATTERN_TS,
    KEY_EXPRESSION_JS,
    KEY_EXPRESSION_TS,
    TAG_EXPRESSION_JS,
    TAG_EXPRESSION_TS,
    DECLARATION_BLOCK_OPEN,
    DECLARATION_EXPRESSION_JS,
    DECLARATION_EXPRESSION_TS,
    SNIPPET_PARAMETER_JS,
    SNIPPET_PARAMETER_TS,
    SNIPPET_TYPE_PARAMS,
    BLOCK_END_OPEN,
    SNIPPET_NAME,
    SNIPPET_NAME_NO_PARAMS,
    SNIPPET_NAME_MISSING,
    BLOCK_START_EOF,
    BLOCK_EOF,
    SNIPPET_HEADER_TRAILING,
    INCOMPLETE_ATTRIBUTE_EXPRESSION,
};

typedef struct {
    uint64_t svelte_scan_calls;
    uint64_t htmlx_fallback_calls;
    uint64_t scan_lt_as_tag_boundary_calls;
    uint64_t scan_lt_as_tag_boundary_successes;
    uint64_t scan_lt_as_tag_boundary_bytes;
    uint64_t scan_balanced_calls;
    uint64_t scan_balanced_successes;
    uint64_t scan_balanced_bytes;
    uint64_t scan_iterator_calls;
    uint64_t scan_iterator_successes;
    uint64_t scan_iterator_bytes;
    uint64_t scan_binding_calls;
    uint64_t scan_binding_successes;
    uint64_t scan_key_calls;
    uint64_t scan_key_successes;
    uint64_t scan_tag_expression_calls;
    uint64_t scan_tag_expression_successes;
    uint64_t scan_tag_expression_bytes;
    uint64_t scan_declaration_expression_calls;
    uint64_t scan_declaration_expression_successes;
    uint64_t scan_snippet_parameter_calls;
    uint64_t scan_snippet_parameter_successes;
    uint64_t scan_snippet_type_params_calls;
    uint64_t scan_snippet_type_params_successes;
    uint64_t scan_snippet_type_params_bytes;
    uint64_t scan_snippet_name_calls;
    uint64_t scan_snippet_name_successes;
    uint64_t scan_snippet_name_bytes;
    uint64_t scan_block_end_open_calls;
    uint64_t scan_block_end_open_successes;
    uint64_t scan_block_end_open_bytes;
} SvelteScannerProfileStats;

static SvelteScannerProfileStats s_profile_stats;

#ifdef TREE_SITTER_SVELTE_PROFILE
#define PROFILE_COUNT(field) (++s_profile_stats.field)
#define PROFILE_ADVANCE(field, lexer) do { ++s_profile_stats.field; advance(lexer); } while (0)
#define PROFILE_SKIP(field, lexer) do { ++s_profile_stats.field; skip(lexer); } while (0)
#else
#define PROFILE_COUNT(field) ((void)0)
#define PROFILE_ADVANCE(field, lexer) advance(lexer)
#define PROFILE_SKIP(field, lexer) skip(lexer)
#endif

bool tree_sitter_svelte_profile_enabled(void) {
#ifdef TREE_SITTER_SVELTE_PROFILE
    return true;
#else
    return false;
#endif
}

void tree_sitter_svelte_profile_reset(void) {
    memset(&s_profile_stats, 0, sizeof(s_profile_stats));
}

void tree_sitter_svelte_profile_snapshot(SvelteScannerProfileStats *out) {
    if (out != NULL) {
        *out = s_profile_stats;
    }
}

static bool is_svelte_tag_name_char(int32_t c) {
    return is_ident_char(c) || c == '-' || c == ':' || c == '.';
}

static inline int svelte_js_ts_symbol(State *state, int js_symbol, int ts_symbol) {
    return state->is_typescript ? ts_symbol : js_symbol;
}

static bool scan_lt_as_tag_boundary(TSLexer *lexer) {
    PROFILE_COUNT(scan_lt_as_tag_boundary_calls);
    PROFILE_ADVANCE(scan_lt_as_tag_boundary_bytes, lexer);

    if (lexer->lookahead == '/' || lexer->lookahead == '!') {
        PROFILE_COUNT(scan_lt_as_tag_boundary_successes);
        return true;
    }

    if (!is_alpha(lexer->lookahead)) {
        return false;
    }

    while (is_svelte_tag_name_char(lexer->lookahead)) {
        PROFILE_ADVANCE(scan_lt_as_tag_boundary_bytes, lexer);
    }

    if (lexer->lookahead == '>') {
        PROFILE_ADVANCE(scan_lt_as_tag_boundary_bytes, lexer);
        if (lexer->lookahead != '(') {
            PROFILE_COUNT(scan_lt_as_tag_boundary_successes);
            return true;
        }
        return false;
    }

    if (lexer->lookahead == '/') {
        PROFILE_ADVANCE(scan_lt_as_tag_boundary_bytes, lexer);
        if (lexer->lookahead == '>') {
            PROFILE_COUNT(scan_lt_as_tag_boundary_successes);
            return true;
        }
        return false;
    }

    if (!is_space(lexer->lookahead)) {
        return false;
    }

    while (is_space(lexer->lookahead)) {
        PROFILE_ADVANCE(scan_lt_as_tag_boundary_bytes, lexer);
    }

    bool is_boundary = lexer->lookahead == '>'
        || lexer->lookahead == '/'
        || lexer->lookahead == '{'
        || lexer->lookahead == '"'
        || lexer->lookahead == '\''
        || lexer->lookahead == '|'
        || is_ident_start(lexer->lookahead);
    if (is_boundary) {
        PROFILE_COUNT(scan_lt_as_tag_boundary_successes);
    }
    return is_boundary;
}

// Scan balanced expression with trailing whitespace exclusion, comment handling,
// tag boundary detection, and Svelte block marker detection.
// Used for iterator expressions, binding patterns, key expressions, tag expressions,
// declaration expressions, and snippet parameters.
static bool scan_balanced(TSLexer *lexer, int32_t stop_char, bool stop_comma, bool stop_semicolon, bool allow_eof) {
    PROFILE_COUNT(scan_balanced_calls);
    int depth = 0;
    bool has_content = false;
    bool needs_mark = false;
    bool found_terminator = false;

    while (lexer->lookahead) {
        int32_t c = lexer->lookahead;

        if (depth == 0 && (c == stop_char || c == '}')) {
            found_terminator = true;
            break;
        }
        if (depth == 0 && stop_comma && c == ',') {
            found_terminator = true;
            break;
        }
        if (depth == 0 && stop_semicolon && c == ';') {
            found_terminator = true;
            break;
        }
        // At depth 0, classify '<' as either tag boundary or expression operator.
        if (depth == 0 && c == '<') {
            if (needs_mark) {
                lexer->mark_end(lexer);
                needs_mark = false;
            }

            if (scan_lt_as_tag_boundary(lexer)) {
                found_terminator = true;
                break;
            }

            has_content = true;
            needs_mark = true;
            continue;
        }

        if (skip_string(lexer)) {
            has_content = true;
            needs_mark = true;
            continue;
        }

        // JS comments: // line and /* block */
        if (c == '/') {
            advance(lexer);

            if (lexer->lookahead == '/') {
                advance(lexer);
                while (lexer->lookahead && lexer->lookahead != '\n' && lexer->lookahead != '\r') {
                    PROFILE_ADVANCE(scan_balanced_bytes, lexer);
                }
                has_content = true;
                needs_mark = true;
                continue;
            }

            if (lexer->lookahead == '*') {
                PROFILE_ADVANCE(scan_balanced_bytes, lexer);
                while (lexer->lookahead) {
                    if (lexer->lookahead != '*') {
                        PROFILE_ADVANCE(scan_balanced_bytes, lexer);
                        continue;
                    }

                    PROFILE_ADVANCE(scan_balanced_bytes, lexer);
                    if (lexer->lookahead == '/') {
                        PROFILE_ADVANCE(scan_balanced_bytes, lexer);
                        break;
                    }
                }
                has_content = true;
                needs_mark = true;
                continue;
            }

            has_content = true;
            needs_mark = true;
            continue;
        }

        // At depth 0, mark before whitespace (handles trailing ws)
        if (depth == 0 && is_space(c)) {
            if (needs_mark) {
                lexer->mark_end(lexer);
                needs_mark = false;
            }
            do { PROFILE_ADVANCE(scan_balanced_bytes, lexer); } while (is_space(lexer->lookahead));
            continue;
        }

        // At depth 0, '{' followed by a Svelte block marker (#, :, /, @)
        // is a block boundary — terminate the balanced scan.
        if (depth == 0 && c == '{') {
            if (needs_mark) {
                lexer->mark_end(lexer);
                needs_mark = false;
            }
            PROFILE_ADVANCE(scan_balanced_bytes, lexer);
            int32_t next = lexer->lookahead;
            if (next == '#' || next == ':' || next == '/' || next == '@') {
                found_terminator = true;
                break;
            }
            // Not a block marker — treat as nesting
            depth++;
            has_content = true;
            needs_mark = true;
            continue;
        }

        switch (c) {
            case '(': case '[': case '{': depth++; break;
            case ')': case ']': case '}': if (--depth < 0) goto done; break;
        }

        PROFILE_ADVANCE(scan_balanced_bytes, lexer);
        has_content = true;
        needs_mark = true;
    }

done:
    if (needs_mark) {
        lexer->mark_end(lexer);
    }

    if (allow_eof && has_content && lexer->lookahead == 0) {
        PROFILE_COUNT(scan_balanced_successes);
        return true;
    }

    bool success = has_content && found_terminator;
    if (success) {
        PROFILE_COUNT(scan_balanced_successes);
    }
    return success;
}

static inline bool match_keyword(TSLexer *lexer, const char *kw, int len) {
    for (int i = 0; i < len; i++) {
        if (lexer->lookahead != kw[i]) return false;
        PROFILE_ADVANCE(scan_iterator_bytes, lexer);
    }
    // Keyword must be followed by space, '{', or '}' (end of block)
    return is_space(lexer->lookahead) || lexer->lookahead == '{' || lexer->lookahead == '}';
}

static inline bool finish_iterator_expression(State *state, TSLexer *lexer, bool result) {
    lexer->result_symbol = svelte_js_ts_symbol(state, ITERATOR_EXPRESSION_JS, ITERATOR_EXPRESSION_TS);
    PROFILE_COUNT(scan_iterator_successes);
    return result;
}

static bool scan_iterator(State *state, TSLexer *lexer) {
    PROFILE_COUNT(scan_iterator_calls);
    int depth = 0;
    bool has_content = false;
    bool found_terminator = false;

    while (is_space(lexer->lookahead)) PROFILE_SKIP(scan_iterator_bytes, lexer);

    // Empty expression: produce zero-width token at the terminator position.
    // This lets the compiler know WHERE the expression would be, even when absent.
    if (lexer->lookahead == '}') {
        lexer->mark_end(lexer);
        return finish_iterator_expression(state, lexer, true);
    }

    while (lexer->lookahead) {
        int32_t c = lexer->lookahead;

        if (depth == 0 && c == '}') {
            found_terminator = true;
            break;
        }
        if (depth == 0 && c == ',') {
            found_terminator = true;
            break;
        }

        // At depth 0, classify '<' as either tag boundary or expression operator.
        if (depth == 0 && c == '<') {
            PROFILE_ADVANCE(scan_iterator_bytes, lexer);
            int32_t next = lexer->lookahead;
            if (next == '/' || next == '!') {
                found_terminator = true;
                break;
            }

            has_content = true;
            continue;
        }

        if (depth == 0 && is_space(c)) {
            lexer->mark_end(lexer);
            while (is_space(lexer->lookahead)) PROFILE_ADVANCE(scan_iterator_bytes, lexer);

            c = lexer->lookahead;
            if (c == '<' && scan_lt_as_tag_boundary(lexer)) {
                return finish_iterator_expression(state, lexer, has_content);
            }
            if (c == 'a') {
                PROFILE_ADVANCE(scan_iterator_bytes, lexer);
                if (match_keyword(lexer, "s", 1)) {
                    return finish_iterator_expression(state, lexer, has_content);
                }
            } else if (c == 't' && match_keyword(lexer, "then", 4)) {
                return finish_iterator_expression(state, lexer, has_content);
            } else if (c == 'c' && match_keyword(lexer, "catch", 5)) {
                return finish_iterator_expression(state, lexer, has_content);
            } else if (c == '(') {
                return finish_iterator_expression(state, lexer, has_content);
            } else if (c == 0) {
                return finish_iterator_expression(state, lexer, has_content);
            }
            has_content = true;
            continue;
        }

        if (skip_string(lexer)) {
            has_content = true;
            continue;
        }

        switch (c) {
            case '(': case '[': case '{': depth++; break;
            case ')': case ']': case '}': depth--; break;
        }

        PROFILE_ADVANCE(scan_iterator_bytes, lexer);
        has_content = true;
    }

    // Only return true if we found a valid terminator
    if (has_content && (found_terminator || lexer->lookahead == 0)) {
        lexer->mark_end(lexer);
        return finish_iterator_expression(state, lexer, true);
    }
    return false;
}

static bool scan_balanced_svelte_token(
    State *state,
    TSLexer *lexer,
    int32_t stop_char,
    bool stop_comma,
    bool stop_semicolon,
    bool allow_eof,
    int js_symbol,
    int ts_symbol
) {
    while (is_space(lexer->lookahead)) PROFILE_SKIP(scan_balanced_bytes, lexer);
    if (!scan_balanced(lexer, stop_char, stop_comma, stop_semicolon, allow_eof)) return false;

    lexer->result_symbol = svelte_js_ts_symbol(state, js_symbol, ts_symbol);
    return true;
}

static bool scan_binding(State *state, TSLexer *lexer) {
    PROFILE_COUNT(scan_binding_calls);
    if (!scan_balanced_svelte_token(
        state,
        lexer,
        '(',
        true,
        false,
        true,
        BINDING_PATTERN_JS,
        BINDING_PATTERN_TS
    )) return false;

    PROFILE_COUNT(scan_binding_successes);
    return true;
}

static bool scan_key(State *state, TSLexer *lexer) {
    PROFILE_COUNT(scan_key_calls);
    if (!scan_balanced_svelte_token(
        state,
        lexer,
        ')',
        false,
        false,
        true,
        KEY_EXPRESSION_JS,
        KEY_EXPRESSION_TS
    )) return false;

    PROFILE_COUNT(scan_key_successes);
    return true;
}

static bool scan_snippet_parameter(State *state, TSLexer *lexer) {
    PROFILE_COUNT(scan_snippet_parameter_calls);
    if (!scan_balanced_svelte_token(
        state,
        lexer,
        ')',
        true,
        false,
        true,
        SNIPPET_PARAMETER_JS,
        SNIPPET_PARAMETER_TS
    )) return false;

    PROFILE_COUNT(scan_snippet_parameter_successes);
    return true;
}

static bool scan_block_start_eof(TSLexer *lexer) {
    if (lexer->lookahead != 0) return false;

    lexer->mark_end(lexer);
    lexer->result_symbol = BLOCK_START_EOF;
    return true;
}

static bool scan_block_eof(TSLexer *lexer) {
    if (lexer->lookahead != 0) return false;

    lexer->mark_end(lexer);
    lexer->result_symbol = BLOCK_EOF;
    return true;
}

// Tag-like expression: content after a tag keyword. Requires leading whitespace,
// then delegates to scan_balanced.
static bool scan_tag_like_expression(
    State *state,
    TSLexer *lexer,
    int js_symbol,
    int ts_symbol,
    bool stop_semicolon
) {
    bool has_space = false;
    while (is_space(lexer->lookahead)) {
        PROFILE_SKIP(scan_tag_expression_bytes, lexer);
        has_space = true;
    }

    if (!has_space) return false;

    // Empty expression: produce zero-width token at the terminator position.
    if (lexer->lookahead == '}') {
        lexer->mark_end(lexer);
        lexer->result_symbol = svelte_js_ts_symbol(state, js_symbol, ts_symbol);
        return true;
    }

    if (!scan_balanced(lexer, '}', false, stop_semicolon, false)) return false;

    lexer->result_symbol = svelte_js_ts_symbol(state, js_symbol, ts_symbol);
    return true;
}

// Tag expression: content after {@html, {@debug, etc.
static bool scan_tag_expression(State *state, TSLexer *lexer) {
    PROFILE_COUNT(scan_tag_expression_calls);
    if (!scan_tag_like_expression(
        state,
        lexer,
        TAG_EXPRESSION_JS,
        TAG_EXPRESSION_TS,
        false
    )) return false;

    PROFILE_COUNT(scan_tag_expression_successes);
    return true;
}

// Declaration expression: content after {let or {const. A top-level semicolon is
// a declaration-tag boundary so `{let x = 1;}` does not parse as a clean tag.
static bool scan_declaration_expression(State *state, TSLexer *lexer) {
    PROFILE_COUNT(scan_declaration_expression_calls);
    if (!scan_tag_like_expression(
        state,
        lexer,
        DECLARATION_EXPRESSION_JS,
        DECLARATION_EXPRESSION_TS,
        true
    )) return false;

    PROFILE_COUNT(scan_declaration_expression_successes);
    return true;
}

static bool scan_incomplete_attribute_expression(TSLexer *lexer) {
    if (lexer->lookahead != '{') return false;

    lexer->mark_end(lexer);
    advance(lexer);

    for (;;) {
        while (is_space(lexer->lookahead)) {
            advance(lexer);
            lexer->mark_end(lexer);
        }

        if (lexer->lookahead == '/') {
            advance(lexer);

            if (lexer->lookahead == '/') {
                advance(lexer);
                while (lexer->lookahead && lexer->lookahead != '\n' && lexer->lookahead != '\r') {
                    advance(lexer);
                }
                lexer->mark_end(lexer);
                continue;
            }

            if (lexer->lookahead == '*') {
                advance(lexer);
                while (lexer->lookahead) {
                    if (lexer->lookahead != '*') {
                        advance(lexer);
                        continue;
                    }
                    advance(lexer);
                    if (lexer->lookahead == '/') {
                        advance(lexer);
                        break;
                    }
                }
                lexer->mark_end(lexer);
                continue;
            }

            if (lexer->lookahead == '>') {
                lexer->result_symbol = INCOMPLETE_ATTRIBUTE_EXPRESSION;
                return true;
            }

            return false;
        }

        if (lexer->lookahead == '>') {
            lexer->result_symbol = INCOMPLETE_ATTRIBUTE_EXPRESSION;
            return true;
        }

        return false;
    }
}

static bool scan_snippet_header_trailing(TSLexer *lexer) {
    while (is_space(lexer->lookahead)) {
        advance(lexer);
    }

    if (lexer->lookahead == 0 || lexer->lookahead == '<' || lexer->lookahead == '{' || lexer->lookahead == '}' || lexer->lookahead == '(') {
        return false;
    }

    while (lexer->lookahead && lexer->lookahead != '<' && lexer->lookahead != '{' && lexer->lookahead != '}') {
        advance(lexer);
    }

    lexer->mark_end(lexer);
    lexer->result_symbol = SNIPPET_HEADER_TRAILING;
    return true;
}

static bool scan_snippet_type_params(TSLexer *lexer) {
    PROFILE_COUNT(scan_snippet_type_params_calls);
    if (lexer->lookahead != '<') return false;

    int depth = 0;

    while (lexer->lookahead) {
        int32_t c = lexer->lookahead;

        if (skip_string(lexer)) {
            continue;
        }

        if (c == '<') {
            depth++;
            PROFILE_ADVANCE(scan_snippet_type_params_bytes, lexer);
            continue;
        }

        if (c == '>') {
            depth--;
            PROFILE_ADVANCE(scan_snippet_type_params_bytes, lexer);
            if (depth == 0) {
                lexer->mark_end(lexer);
                lexer->result_symbol = SNIPPET_TYPE_PARAMS;
                PROFILE_COUNT(scan_snippet_type_params_successes);
                return true;
            }
            continue;
        }

        PROFILE_ADVANCE(scan_snippet_type_params_bytes, lexer);
    }

    return false;
}

static bool scan_snippet_name_with_terminator(
    TSLexer *lexer,
    int result_symbol,
    bool allow_params,
    bool allow_type_params,
    bool allow_close,
    bool allow_eof
) {
    PROFILE_COUNT(scan_snippet_name_calls);
    while (is_space(lexer->lookahead)) PROFILE_SKIP(scan_snippet_name_bytes, lexer);

    if (!is_ident_start(lexer->lookahead) && lexer->lookahead != '$') return false;

    PROFILE_ADVANCE(scan_snippet_name_bytes, lexer);
    while (is_ident_char(lexer->lookahead) || lexer->lookahead == '$') {
        PROFILE_ADVANCE(scan_snippet_name_bytes, lexer);
    }

    lexer->mark_end(lexer);
    while (is_space(lexer->lookahead)) PROFILE_ADVANCE(scan_snippet_name_bytes, lexer);

    bool valid_terminator =
        (allow_params && lexer->lookahead == '(') ||
        (allow_type_params && lexer->lookahead == '<') ||
        (allow_close && lexer->lookahead == '}') ||
        (allow_eof && lexer->lookahead == 0);
    if (!valid_terminator) return false;

    lexer->result_symbol = result_symbol;
    PROFILE_COUNT(scan_snippet_name_successes);
    return true;
}

// Snippet name: identifier or zero-width token when name is absent.
static bool scan_snippet_name(TSLexer *lexer) {
    return scan_snippet_name_with_terminator(lexer, SNIPPET_NAME, true, true, false, false);
}

static bool scan_snippet_name_no_params(TSLexer *lexer) {
    return scan_snippet_name_with_terminator(lexer, SNIPPET_NAME_NO_PARAMS, false, false, true, true);
}

static bool scan_snippet_name_missing(TSLexer *lexer) {
    while (is_space(lexer->lookahead)) PROFILE_SKIP(scan_snippet_name_bytes, lexer);
    if (lexer->lookahead != '}' && lexer->lookahead != 0) return false;
    lexer->mark_end(lexer);
    lexer->result_symbol = SNIPPET_NAME_MISSING;
    return true;
}

static bool scan_declaration_keyword(TSLexer *lexer, const char *keyword, unsigned length) {
    for (unsigned i = 0; i < length; i++) {
        if (lexer->lookahead != keyword[i]) return false;
        advance(lexer);
    }

    return !is_ident_char(lexer->lookahead);
}

// Match declaration-tag `{` and block-end `{/` openings from one lookahead path.
// The declaration token ends at `{`; its keyword is consumed later by the grammar.
static bool scan_svelte_brace_open(
    State *state,
    TSLexer *lexer,
    bool declaration_valid,
    bool block_end_valid,
    bool unterminated_tag_end_valid
) {
    PROFILE_COUNT(scan_block_end_open_calls);
    if (lexer->lookahead != '{') return false;
    lexer->mark_end(lexer);
    PROFILE_ADVANCE(scan_block_end_open_bytes, lexer);

    if (block_end_valid && lexer->lookahead == '/') {
        PROFILE_ADVANCE(scan_block_end_open_bytes, lexer);
        if (!is_ident_start(lexer->lookahead)) return false;
        lexer->mark_end(lexer);
        lexer->result_symbol = BLOCK_END_OPEN;
        PROFILE_COUNT(scan_block_end_open_successes);
        return true;
    }

    if (unterminated_tag_end_valid && htmlx_has_open_tag(state) && scan_block_boundary_after_open(state, lexer)) {
        return true;
    }

    if (!declaration_valid) return false;

    lexer->mark_end(lexer);
    while (is_space(lexer->lookahead)) advance(lexer);

    bool matches =
        (lexer->lookahead == 'l' && scan_declaration_keyword(lexer, "let", 3)) ||
        (lexer->lookahead == 'c' && scan_declaration_keyword(lexer, "const", 5));
    if (!matches) return false;

    lexer->result_symbol = DECLARATION_BLOCK_OPEN;
    return true;
}

// Main dispatch for Svelte external scanner.
//
// BLOCK_END_OPEN uses early-return (return result regardless of success/failure)
// because it advances the lexer on failure. When it returns false, tree-sitter
// falls back to its internal lexer at the original position, matching literal
// tokens like { and {:
//
// Other svelte tokens use the same early-return pattern because they're
// expected in exclusive grammar contexts where no other token competes.
static bool svelte_scan(State *state, TSLexer *lexer, const bool *valid) {
    PROFILE_COUNT(svelte_scan_calls);
    // BLOCK_END_OPEN: disambiguates {/if} (block end) from {/* comment */} (expression).
    // Only attempt when lookahead is '{' — otherwise fall through to HTMLX scanner
    // so it can produce TEXT and other tokens that don't start with '{'.
    if (scan_void_end(state, lexer, valid)) {
        return true;
    }

    if ((valid[DECLARATION_BLOCK_OPEN] || valid[BLOCK_END_OPEN]) && lexer->lookahead == '{') {
        return scan_svelte_brace_open(
            state,
            lexer,
            valid[DECLARATION_BLOCK_OPEN],
            valid[BLOCK_END_OPEN],
            valid[UNTERMINATED_TAG_END]
        );
    }
    if (valid[BLOCK_START_EOF] && lexer->lookahead == 0) return scan_block_start_eof(lexer);
    if (valid[BLOCK_EOF] && lexer->lookahead == 0) return scan_block_eof(lexer);

    // Svelte block expression tokens — each valid in exclusive grammar contexts.
    // SNIPPET_NAME must be checked before SNIPPET_TYPE_PARAMS and SNIPPET_PARAMETER
    // because all three can be valid simultaneously (all are optional in the grammar).
    if (valid[SNIPPET_NAME] || valid[SNIPPET_NAME_NO_PARAMS] || valid[SNIPPET_NAME_MISSING]) {
        if (scan_snippet_name(lexer)) return true;
        if (valid[SNIPPET_NAME_NO_PARAMS] && scan_snippet_name_no_params(lexer)) return true;
        if (valid[SNIPPET_NAME_MISSING] && scan_snippet_name_missing(lexer)) return true;
        if (valid[SNIPPET_NAME]) return false;
    }
    if (valid[SNIPPET_HEADER_TRAILING]) return scan_snippet_header_trailing(lexer);
    if (valid[SNIPPET_TYPE_PARAMS]) return scan_snippet_type_params(lexer);
    if (valid[SNIPPET_PARAMETER_JS] || valid[SNIPPET_PARAMETER_TS]) return scan_snippet_parameter(state, lexer);
    if (valid[ITERATOR_EXPRESSION_JS] || valid[ITERATOR_EXPRESSION_TS]) return scan_iterator(state, lexer);
    if (valid[BINDING_PATTERN_JS] || valid[BINDING_PATTERN_TS]) return scan_binding(state, lexer);
    if (valid[KEY_EXPRESSION_JS] || valid[KEY_EXPRESSION_TS]) return scan_key(state, lexer);
    if (valid[TAG_EXPRESSION_JS] || valid[TAG_EXPRESSION_TS]) return scan_tag_expression(state, lexer);
    if (valid[DECLARATION_EXPRESSION_JS] || valid[DECLARATION_EXPRESSION_TS]) return scan_declaration_expression(state, lexer);
    if (valid[INCOMPLETE_ATTRIBUTE_EXPRESSION] && lexer->lookahead == '{') {
        return scan_incomplete_attribute_expression(lexer);
    }

    // Fall through to HTMLX scanner for all other tokens.
    PROFILE_COUNT(htmlx_fallback_calls);
    return scan(state, lexer, valid);
}

void *tree_sitter_svelte_external_scanner_create(void) {
    return htmlx_create();
}

void tree_sitter_svelte_external_scanner_destroy(void *payload) {
    htmlx_destroy(payload);
}

unsigned tree_sitter_svelte_external_scanner_serialize(void *payload, char *buffer) {
    return htmlx_serialize(payload, buffer);
}

void tree_sitter_svelte_external_scanner_deserialize(void *payload, const char *buffer, unsigned length) {
    htmlx_deserialize(payload, buffer, length);
}

bool tree_sitter_svelte_external_scanner_scan(void *payload, TSLexer *lexer, const bool *valid) {
    return svelte_scan(payload, lexer, valid);
}
