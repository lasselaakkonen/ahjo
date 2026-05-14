package ahjocontainer

import "os"

func mkdirAll(p string) error     { return os.MkdirAll(p, 0o755) }
func writeFile(p, b string) error { return os.WriteFile(p, []byte(b), 0o644) }
