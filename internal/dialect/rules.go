package dialect

// Rule is one translation rule (docs/05-dialect.md section 1). The registry order is the
// application order; docs/agent-notes/rule-order.md documents it and a test pins both.
type Rule struct {
	ID      string
	Name    string
	Summary string
}

const (
	ruleVersionComment = "R001"
	ruleStringParam    = "R002"
	ruleNullSafeEqual  = "R003"
	ruleDBQualifier    = "R004"
	ruleLimitOffset    = "R005"
	ruleDefaultValues  = "R006"
	ruleUpdateLimit    = "R007"
	ruleHintStrip      = "R008"
)

var rules = []Rule{
	{ruleVersionComment, "version-comment-gate", "blank /*!NNNNN ... */ above the server version and /*T! ... */ before parsing"},
	{ruleStringParam, "string-literal-param", "bind string literals as parameters, after MySQL unescaping"},
	{ruleNullSafeEqual, "null-safe-equal", "a <=> b becomes a IS b"},
	{ruleDBQualifier, "database-qualifier", "resolve db.table to the database file and drop the qualifier"},
	{ruleLimitOffset, "limit-offset", "LIMIT o, n becomes LIMIT n OFFSET o; the maximum unsigned count becomes -1"},
	{ruleDefaultValues, "insert-default-values", "INSERT ... VALUES () becomes INSERT ... DEFAULT VALUES"},
	{ruleUpdateLimit, "update-delete-limit", "UPDATE/DELETE with ORDER BY or LIMIT filter by a rowid subquery"},
	{ruleHintStrip, "hint-strip", "drop optimizer hints, index hints and priority modifiers"},
}

// Rules returns the registry in application order.
func Rules() []Rule {
	return append([]Rule(nil), rules...)
}
