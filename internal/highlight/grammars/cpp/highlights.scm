(identifier) @variable

((identifier) @constant
  (#match? @constant "^[A-Z][A-Z\\d_]*$"))

[
  "sizeof"
  "offsetof"
  "alignof"
  "_Alignof"
  "asm"
  "__asm__"
] @keyword

[
  "enum"
  "struct"
  "typedef"
  "union"
] @keyword.storage.type

[
  (type_qualifier)
  (storage_class_specifier)
] @keyword.storage.modifier

[
  "goto"
  "break"
  "continue"
] @keyword.control

[
  "do"
  "for"
  "while"
] @keyword.control.repeat

[
  "if"
  "else"
  "switch"
  "case"
  "default"
] @keyword.control.conditional

"return" @keyword.control.return

[
  "defined"
  "#define"
  "#elif"
  "#else"
  "#endif"
  "#if"
  "#ifdef"
  "#ifndef"
  "#elifdef"
  "#elifndef"
  "#include"
  (preproc_directive)
] @keyword.directive

"..." @punctuation

["," "." ":" "::" ";" "->"] @punctuation.delimiter

["(" ")" "[" "]" "{" "}" "[[" "]]"] @punctuation.bracket

[
  "+"
  "-"
  "*"
  "/"
  "++"
  "--"
  "%"
  "=="
  "!="
  ">"
  "<"
  ">="
  "<="
  "&&"
  "||"
  "!"
  "&"
  "|"
  "^"
  "~"
  "<<"
  ">>"
  "="
  "+="
  "-="
  "*="
  "/="
  "%="
  "<<="
  ">>="
  "&="
  "^="
  "|="
  "?"
] @operator

(conditional_expression ":" @operator) ; After punctuation

(pointer_declarator "*" @type.builtin) ; After Operators
(abstract_pointer_declarator "*" @type.builtin)


[(true) (false)] @constant.builtin.boolean

; C enumerators are integer constants; colour the definition the same as a use
; site (a SCREAMING_SNAKE reference resolves to @constant above), not as a
; distinct enum variant. C++ overrides this to keep @type.enum.variant.
(enumerator name: (identifier) @constant)

(string_literal) @string
(system_lib_string) @string

(null) @constant.builtin
(number_literal) @constant.numeric
(char_literal) @constant.character
(escape_sequence) @constant.character.escape

(field_identifier) @variable.other.member
(statement_identifier) @label
(type_identifier) @type
(primitive_type) @type.builtin
(sized_type_specifier) @type.builtin

; `typedef ... Name;` — the introduced name
(type_definition
  declarator: (type_identifier) @type.definition)

(call_expression
  function: (identifier) @function)
(call_expression
  function: (field_expression
    field: (field_identifier) @function))
(call_expression (argument_list (identifier) @variable))
(function_declarator
  declarator: [(identifier) (field_identifier)] @function)

; GCC builtins, e.g. __builtin_expect
((call_expression
  function: (identifier) @function.builtin)
 (#match? @function.builtin "^__builtin_"))

; Up to 6 layers of declarators
(parameter_declaration
  declarator: (identifier) @variable.parameter)
(parameter_declaration
  (_
    (identifier) @variable.parameter))
(parameter_declaration
  (_
    (_
      (identifier) @variable.parameter)))
(parameter_declaration
  (_
    (_
      (_
        (identifier) @variable.parameter))))
(parameter_declaration
  (_
    (_
      (_
        (_
          (identifier) @variable.parameter)))))
(parameter_declaration
  (_
    (_
      (_
        (_
          (_
            (identifier) @variable.parameter))))))

(preproc_function_def
  name: (identifier) @function.special)

(attribute
  name: (identifier) @attribute)

; GNU / MSVC attributes and calling conventions
[
  "__attribute__"
  "__declspec"
  "__based"
  "__cdecl"
  "__clrcall"
  "__stdcall"
  "__fastcall"
  "__thiscall"
  "__vectorcall"
] @attribute

; Builtin/predefined constants and macros.
((identifier) @constant.builtin
 (#any-of? @constant.builtin
   "stderr" "stdin" "stdout"
   "__FILE__" "__LINE__" "__DATE__" "__TIME__" "__func__"
   "__FUNCTION__" "__PRETTY_FUNCTION__" "__BASE_FILE__"
   "__STDC__" "__STDC_VERSION__" "__STDC_HOSTED__"
   "__VA_ARGS__" "__VA_OPT__" "__cplusplus"))

(comment) @comment


; Constants

(this) @variable.builtin
(null) @constant.builtin

; Types

(using_declaration ("using" "namespace" (identifier) @namespace))
(using_declaration ("using" "namespace" (qualified_identifier name: (identifier) @namespace)))
; Only a Capitalised qualified leaf (`Color::Red`) is an enum variant; a
; lowercase one (`std::cout`) is a value/member and falls through to @variable.
((qualified_identifier name: (identifier) @type.enum.variant)
 (#match? @type.enum.variant "^[A-Z]"))
; C colours enumerator definitions @constant (see c/highlights.scm); C++ resolves
; enum values through the qualified path above as @type.enum.variant, so keep the
; definition matching that.
(enumerator name: (identifier) @type.enum.variant)
(namespace_definition name: (namespace_identifier) @namespace)
(namespace_identifier) @namespace

; Type-introducing declarations
(concept_definition name: (identifier) @type.definition)
(alias_declaration name: (type_identifier) @type.definition)

(auto) @type.builtin

(ref_qualifier ["&" "&&"] @type.builtin)
(reference_declarator ["&" "&&"] @type.builtin)
(abstract_reference_declarator ["&" "&&"] @type.builtin)

; -------
; Functions
; -------
; Support up to 4 levels of nesting of qualifiers
; i.e. a::b::c::d::func();
(call_expression
  function: (qualified_identifier
    name: (identifier) @function))
(call_expression
  function: (qualified_identifier
    name: (qualified_identifier
      name: (identifier) @function)))
(call_expression
  function: (qualified_identifier
    name: (qualified_identifier
      name: (qualified_identifier
        name: (identifier) @function))))
(call_expression
  function: (qualified_identifier
    name: (qualified_identifier
      name: (qualified_identifier
        name: (qualified_identifier
          name: (identifier) @function)))))

(template_function
  name: (identifier) @function)

(template_method
  name: (field_identifier) @function)

; Support up to 4 levels of nesting of qualifiers
; i.e. a::b::c::d::func();
(function_declarator
  declarator: (qualified_identifier
    name: (identifier) @function))
(function_declarator
  declarator: (qualified_identifier
    name: (qualified_identifier
      name: (identifier) @function)))
(function_declarator
  declarator: (qualified_identifier
    name: (qualified_identifier
      name: (qualified_identifier
        name: (identifier) @function))))
(function_declarator
  declarator: (qualified_identifier
    name: (qualified_identifier
      name: (qualified_identifier
        name: (qualified_identifier
          name: (identifier) @function)))))

(function_declarator
  declarator: (field_identifier) @function)

; Constructors

(class_specifier
  (type_identifier) @type
  (field_declaration_list
    (function_definition
      (function_declarator
        (identifier) @constructor)))
        (#eq? @type @constructor)) 
(destructor_name "~" @constructor
  (identifier) @constructor)

; Parameters

(parameter_declaration
  declarator: (reference_declarator (identifier) @variable.parameter))
(optional_parameter_declaration
  declarator: (identifier) @variable.parameter)

; Keywords

(template_argument_list (["<" ">"] @punctuation.bracket))
(template_parameter_list (["<" ">"] @punctuation.bracket))
(default_method_clause "default" @keyword)

"static_assert" @function.special

[
  "<=>"
  "[]"
  "()"
] @operator


; These casts are parsed as function calls, but are not.
((identifier) @keyword (#eq? @keyword "static_cast"))
((identifier) @keyword (#eq? @keyword "dynamic_cast"))
((identifier) @keyword (#eq? @keyword "reinterpret_cast"))
((identifier) @keyword (#eq? @keyword "const_cast"))

[
  "co_await"
  "co_return"
  "co_yield"
  "concept"
  "delete"
  "new"
  "operator"
  "requires"
  "using"
] @keyword

[
  "catch"
  "noexcept"
  "throw"
  "try"
] @keyword.control.exception


[
  "and"
  "and_eq"
  "bitor"
  "bitand"
  "not"
  "not_eq"
  "or"
  "or_eq"
  "xor"
  "xor_eq"
] @keyword.operator

[
  "class"  
  "namespace"
  "typename"
  "template"
] @keyword.storage.type

[
  "constexpr"
  "constinit"
  "consteval"
  "mutable"
] @keyword.storage.modifier

; Modifiers that aren't plausibly type/storage related.
[
  "decltype"
  "explicit"
  "friend"
  "virtual"
  (virtual_specifier) ; override/final
  "private"
  "protected"
  "public"
  "inline" ; C++ meaning differs from C!
] @keyword

; Strings

(raw_string_literal) @string
