package jsbridge

import _ "embed"

// embeddedWorker is worker.cjs, shipped inside the binary so the harness needs
// no external script file at run time.
//
//go:embed worker.cjs
var embeddedWorker []byte
