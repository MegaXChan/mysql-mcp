package policy

import (
	"strconv"

	"vitess.io/vitess/go/vt/sqlparser"
)

// dangerousFunctions is checked before the allowlist so callers receive a
// precise audit reason. Some entries are intentionally redundant with the
// default-deny behavior: an accidental future allowlist expansion must not
// make a blocking, file-reading, or session-mutating function executable.
var dangerousFunctions = map[string]struct{}{
	"benchmark":                         {},
	"get_lock":                          {},
	"is_free_lock":                      {},
	"is_used_lock":                      {},
	"load_file":                         {},
	"master_pos_wait":                   {},
	"release_all_locks":                 {},
	"release_lock":                      {},
	"service_get_read_locks":            {},
	"service_get_write_locks":           {},
	"service_release_locks":             {},
	"sleep":                             {},
	"sys_get_config":                    {},
	"wait_for_executed_gtid_set":        {},
	"wait_until_sql_thread_after_gtids": {},
}

// whitespaceSensitiveBuiltinFunctions is MySQL's documented SYM_FN token
// class. Unless IGNORE_SPACE is enabled, whitespace between one of these names
// and "(" makes the lexer emit an ordinary identifier. These special grammar
// functions are not all present in MySQL's generic native-function registry,
// so resolution can reach a same-named routine in the current database. Vitess
// intentionally normalizes that lexical distinction away in the AST, so
// ParseOne preserves it with a SQL-aware raw-input check before trusting a
// generic FuncExpr or a dedicated built-in node.
var whitespaceSensitiveBuiltinFunctions = map[string]struct{}{
	"adddate": {}, "bit_and": {}, "bit_or": {}, "bit_xor": {},
	"cast": {}, "count": {}, "curdate": {}, "curtime": {},
	"date_add": {}, "date_sub": {}, "extract": {}, "group_concat": {},
	"max": {}, "mid": {}, "min": {}, "now": {}, "position": {},
	"session_user": {}, "std": {}, "stddev": {}, "stddev_pop": {},
	"stddev_samp": {}, "subdate": {}, "substr": {}, "substring": {},
	"sum": {}, "sysdate": {}, "system_user": {}, "trim": {},
	"variance": {}, "var_pop": {}, "var_samp": {},
}

const (
	// MySQL's first native JSON functions were introduced in 5.7.8. Keeping
	// the exact patch boundary matters because on an older server an
	// unqualified unknown name can resolve to a stored function instead.
	mysqlVersionJSONCore = 50708
	// ANY_VALUE() was introduced during the MySQL 5.7 series.
	mysqlVersionAnyValue = 50705
	// These JSON additions were backported together in MySQL 5.7.22.
	mysqlVersionJSON57Additions = 50722
	// Roles, and therefore CURRENT_ROLE(), are a MySQL 8 feature.
	mysqlVersionRoles = 80000
	// Later JSON features use their documented MySQL 8 patch boundaries.
	mysqlVersionJSONStorageFree = 80002
	mysqlVersionWindowFunctions = 80002
	mysqlVersionJSONTable       = 80004
	mysqlVersionRegexpFunctions = 80004
	mysqlVersionJSONWindow      = 80014
	mysqlVersionPFSFunctions    = 80016
	mysqlVersionJSONAdvanced    = 80017
	mysqlVersionJSONValue       = 80021
)

// commonSafeBuiltinFunctions is the deliberately conservative set of
// unqualified, side-effect-free FuncExpr built-ins available throughout the
// supported MySQL families. Unknown names are assumed to be stored functions
// and must use the dedicated mysql.function.call tool. Functions introduced in
// a particular server release belong in versionedSafeBuiltinFunctions instead.
var commonSafeBuiltinFunctions = map[string]struct{}{
	// Numeric functions.
	"abs": {}, "acos": {}, "asin": {}, "atan": {}, "atan2": {},
	"ceil": {}, "ceiling": {}, "conv": {}, "cos": {}, "cot": {},
	"crc32": {}, "degrees": {}, "exp": {}, "floor": {}, "ln": {},
	"log": {}, "log10": {}, "log2": {}, "mod": {}, "pi": {},
	"pow": {}, "power": {}, "radians": {}, "round": {}, "sign": {},
	"sin": {}, "sqrt": {}, "tan": {}, "truncate": {},

	// String and binary conversion functions.
	"ascii": {}, "bin": {}, "bit_length": {}, "char_length": {},
	"character_length": {}, "concat": {}, "concat_ws": {}, "elt": {},
	"export_set": {}, "field": {}, "find_in_set": {}, "format": {},
	"from_base64": {}, "hex": {}, "instr": {}, "lcase": {}, "left": {},
	"length": {}, "locate": {}, "lower": {}, "lpad": {}, "ltrim": {},
	"make_set": {}, "mid": {}, "oct": {}, "octet_length": {}, "ord": {},
	"quote": {}, "repeat": {}, "replace": {}, "reverse": {}, "right": {},
	"rpad": {}, "rtrim": {}, "space": {}, "strcmp": {}, "substr": {},
	"substring": {}, "substring_index": {}, "to_base64": {}, "ucase": {},
	"unhex": {}, "upper": {},

	// Date and time value functions. Blocking replication wait functions are
	// explicitly denied above and are not part of this group.
	"adddate": {}, "addtime": {}, "convert_tz": {}, "date": {},
	"datediff": {}, "date_format": {}, "day": {}, "dayname": {},
	"dayofmonth": {}, "dayofweek": {}, "dayofyear": {}, "from_days": {},
	"from_unixtime": {}, "get_format": {}, "hour": {}, "last_day": {},
	"makedate": {}, "maketime": {}, "microsecond": {}, "minute": {},
	"month": {}, "monthname": {}, "period_add": {}, "period_diff": {},
	"quarter": {}, "sec_to_time": {}, "second": {}, "str_to_date": {},
	"subdate": {}, "subtime": {}, "time": {}, "timediff": {},
	"time_format": {}, "time_to_sec": {}, "to_days": {}, "to_seconds": {},
	"unix_timestamp": {}, "week": {}, "weekday": {}, "weekofyear": {},
	"year": {}, "yearweek": {},

	// Null handling and scalar selection functions.
	"coalesce": {}, "greatest": {}, "if": {}, "ifnull": {},
	"isnull": {}, "least": {}, "nullif": {},

	// Pure digest functions.
	"md5": {}, "sha": {}, "sha1": {}, "sha2": {},

	// Read-only server/session identity functions. None accepts an argument that
	// changes state (LAST_INSERT_ID is intentionally absent for that reason).
	"connection_id": {}, "current_user": {},
	"database": {}, "schema": {}, "session_user": {}, "system_user": {},
	"user": {}, "version": {},
}

// versionedSafeBuiltinFunctions covers version-sensitive functions that
// Vitess can represent as a generic FuncExpr. JSON functions also have
// dedicated AST forms in Vitess v0.24.2; those are checked separately by
// minimumVersionForBuiltinNode using the same release boundaries.
var versionedSafeBuiltinFunctions = map[string]int{
	"json_depth":   mysqlVersionJSONCore,
	"json_length":  mysqlVersionJSONCore,
	"json_type":    mysqlVersionJSONCore,
	"json_valid":   mysqlVersionJSONCore,
	"current_role": mysqlVersionRoles,
}

// mysqlVersionNumber converts Vitess's validated server version into a number
// that preserves major/minor/patch ordering for the supported MySQL 5.7 and 8
// families. Empty input retains the existing parser-default behavior but uses
// the conservative, version-independent function set.
func mysqlVersionNumber(version string) (int, error) {
	if version == "" {
		return 0, nil
	}
	encoded, err := sqlparser.ConvertMySQLVersionToCommentVersion(version)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(encoded)
}

func isDangerousFunction(name string) bool {
	_, found := dangerousFunctions[name]
	return found
}

func isWhitespaceSensitiveBuiltinFunction(name string) bool {
	_, found := whitespaceSensitiveBuiltinFunctions[name]
	return found
}

func isSafeBuiltinFunction(name string, mysqlVersion int) bool {
	if _, found := commonSafeBuiltinFunctions[name]; found {
		return true
	}
	minimum, found := versionedSafeBuiltinFunctions[name]
	return found && mysqlVersion >= minimum
}

// minimumVersionForBuiltinNode identifies version-sensitive built-ins that
// Vitess models with a dedicated AST node instead of FuncExpr. Returning a
// name makes policy errors stable and auditable without echoing caller SQL.
// The zero values mean the node has no version rule in this catalog.
func minimumVersionForBuiltinNode(node sqlparser.SQLNode) (name string, minimum int) {
	// A common aggregate with an OVER clause is a window function. The
	// dedicated JSON aggregates have a later windowing boundary and are
	// handled in their concrete cases below.
	if window, ok := node.(sqlparser.WindowFunc); ok && window.GetOverClause() != nil {
		switch window.(type) {
		case *sqlparser.JSONArrayAgg, *sqlparser.JSONObjectAgg:
			return window.WindowFuncName(), mysqlVersionJSONWindow
		default:
			return window.WindowFuncName(), mysqlVersionWindowFunctions
		}
	}

	switch expression := node.(type) {
	case *sqlparser.AnyValue:
		return "any_value", mysqlVersionAnyValue

	// Core JSON functions introduced with MySQL's JSON type in 5.7.8.
	case *sqlparser.JSONArrayExpr:
		return "json_array", mysqlVersionJSONCore
	case *sqlparser.JSONObjectExpr:
		return "json_object", mysqlVersionJSONCore
	case *sqlparser.JSONQuoteExpr:
		return "json_quote", mysqlVersionJSONCore
	case *sqlparser.JSONContainsExpr:
		return "json_contains", mysqlVersionJSONCore
	case *sqlparser.JSONContainsPathExpr:
		return "json_contains_path", mysqlVersionJSONCore
	case *sqlparser.JSONExtractExpr:
		return "json_extract", mysqlVersionJSONCore
	case *sqlparser.JSONKeysExpr:
		return "json_keys", mysqlVersionJSONCore
	case *sqlparser.JSONSearchExpr:
		return "json_search", mysqlVersionJSONCore
	case *sqlparser.JSONAttributesExpr:
		return jsonAttributeFunctionName(expression.Type), mysqlVersionJSONCore
	case *sqlparser.JSONValueModifierExpr:
		return jsonValueModifierFunctionName(expression.Type), mysqlVersionJSONCore
	case *sqlparser.JSONRemoveExpr:
		return "json_remove", mysqlVersionJSONCore
	case *sqlparser.JSONUnquoteExpr:
		return "json_unquote", mysqlVersionJSONCore
	case *sqlparser.JSONValueMergeExpr:
		if expression.Type == sqlparser.JSONMergeType {
			return "json_merge", mysqlVersionJSONCore
		}
		return jsonValueMergeFunctionName(expression.Type), mysqlVersionJSON57Additions

	// JSON functions added or backported in MySQL 5.7.22.
	case *sqlparser.JSONPrettyExpr:
		return "json_pretty", mysqlVersionJSON57Additions
	case *sqlparser.JSONStorageSizeExpr:
		return "json_storage_size", mysqlVersionJSON57Additions
	case *sqlparser.JSONArrayAgg:
		return "json_arrayagg", mysqlVersionJSON57Additions
	case *sqlparser.JSONObjectAgg:
		return "json_objectagg", mysqlVersionJSON57Additions

	// MySQL 8 patch-level additions. These names could otherwise resolve to a
	// same-named stored function on an older server.
	case *sqlparser.JSONStorageFreeExpr:
		return "json_storage_free", mysqlVersionJSONStorageFree
	case *sqlparser.JSONTableExpr:
		return "json_table", mysqlVersionJSONTable
	case *sqlparser.JSONOverlapsExpr:
		return "json_overlaps", mysqlVersionJSONAdvanced
	case *sqlparser.MemberOfExpr:
		return "member of", mysqlVersionJSONAdvanced
	case *sqlparser.JSONSchemaValidFuncExpr:
		return "json_schema_valid", mysqlVersionJSONAdvanced
	case *sqlparser.JSONSchemaValidationReportFuncExpr:
		return "json_schema_validation_report", mysqlVersionJSONAdvanced
	case *sqlparser.JSONValueExpr:
		return "json_value", mysqlVersionJSONValue

	// MySQL 8 functions that Vitess represents with dedicated nodes.
	case *sqlparser.RegexpInstrExpr:
		return "regexp_instr", mysqlVersionRegexpFunctions
	case *sqlparser.RegexpLikeExpr:
		return "regexp_like", mysqlVersionRegexpFunctions
	case *sqlparser.RegexpReplaceExpr:
		return "regexp_replace", mysqlVersionRegexpFunctions
	case *sqlparser.RegexpSubstrExpr:
		return "regexp_substr", mysqlVersionRegexpFunctions
	case *sqlparser.PerformanceSchemaFuncExpr:
		return performanceSchemaFunctionName(expression.Type), mysqlVersionPFSFunctions
	default:
		return "", 0
	}
}

func performanceSchemaFunctionName(kind sqlparser.PerformanceSchemaType) string {
	switch kind {
	case sqlparser.FormatBytesType:
		return "format_bytes"
	case sqlparser.FormatPicoTimeType:
		return "format_pico_time"
	case sqlparser.PsCurrentThreadIDType:
		return "ps_current_thread_id"
	case sqlparser.PsThreadIDType:
		return "ps_thread_id"
	default:
		return "unknown_performance_schema_function"
	}
}

func jsonAttributeFunctionName(kind sqlparser.JSONAttributeType) string {
	switch kind {
	case sqlparser.DepthAttributeType:
		return "json_depth"
	case sqlparser.TypeAttributeType:
		return "json_type"
	case sqlparser.LengthAttributeType:
		return "json_length"
	case sqlparser.ValidAttributeType:
		return "json_valid"
	default:
		return "unknown_json_attribute"
	}
}

func jsonValueModifierFunctionName(kind sqlparser.JSONValueModifierType) string {
	switch kind {
	case sqlparser.JSONArrayAppendType:
		return "json_array_append"
	case sqlparser.JSONArrayInsertType:
		return "json_array_insert"
	case sqlparser.JSONInsertType:
		return "json_insert"
	case sqlparser.JSONReplaceType:
		return "json_replace"
	case sqlparser.JSONSetType:
		return "json_set"
	default:
		return "unknown_json_modifier"
	}
}

func jsonValueMergeFunctionName(kind sqlparser.JSONValueMergeType) string {
	switch kind {
	case sqlparser.JSONMergePatchType:
		return "json_merge_patch"
	case sqlparser.JSONMergePreserveType:
		return "json_merge_preserve"
	default:
		return "unknown_json_merge"
	}
}
