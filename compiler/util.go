package compiler

func contains(s string, ss []string) bool {
	for _, str := range ss {
		if str == s {
			return true
		}
	}

	return false
}
