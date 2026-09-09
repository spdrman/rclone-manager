package cliecho

// APIAction is one action somebody took through the /api/v1 surface, as
// the engine will record it (issue #599).
//
// It lives here rather than beside the HTTP handlers because core may not
// import apps: a recorder implemented in core/service has to be able to
// name this type, and apps/common/webhost's ActionRecorder interface is
// declared in terms of it. Putting it with the package that already owns
// "what command is this action equivalent to" also keeps one vocabulary
// for one thing.
type APIAction struct {
	// Actor is the authenticated caller's own name, so two operators
	// administering one deployment can tell each other apart. A log that
	// labelled every request as the reader's own would tell them the
	// opposite.
	Actor string

	// Method and Route are the request's own method and the ROUTE
	// PATTERN it matched ("/backup-sets/{source}/{set}"), not the
	// concrete path. The pattern is what identifies the action; the
	// concrete values are in BackupSetID and in the command below.
	Method string
	Route  string

	// Status is the HTTP status the request answered with. Anything from
	// 400 up is a refusal, and a refusal is the case this whole file
	// exists for.
	Status int

	// ErrorCode and Message are the refusal's own words, taken from the
	// error envelope this package already writes, so the terminal says
	// what was refused and why rather than only that something was.
	ErrorCode string
	Message   string

	// BackupSetID is the set this action was about, when it was about
	// one. It decides which feed the line lands on: an event naming a
	// backup set reaches that set's strip, and one naming none reaches
	// the deployment's.
	BackupSetID string

	// Command is the `backup-manager` invocation that would have done the
	// same thing, as Line.Shell renders it: shell-quoted, with no prompt
	// in front and no note after, so what lands in the journal is a
	// command and not a screen. Empty when there is none, and Gap and
	// GapDetail are why there is none.
	Command     string
	Gap         string
	GapDetail   string
	Placeholder bool
}
