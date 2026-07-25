package ollama

import "runtime"

// testFilePath is the absolute path of a file guaranteed to exist on disk (this source
// file), used by TestDetect_DaemonDown_BinaryPresent as a stand-in "binary present" target.
var testFilePath = func() string {
	_, f, _, _ := runtime.Caller(0)
	return f
}()
