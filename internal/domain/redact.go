package domain

const RedactedPlaceholder = "[redacted]"

func RedactArgv(argv []string, sensitive []int) []string {
	out := append([]string(nil), argv...)
	for _, i := range sensitive {
		if i >= 0 && i < len(out) {
			out[i] = RedactedPlaceholder
		}
	}
	return out
}

func RedactEnv(env map[string]string, sensitiveKeys []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	sensitive := make(map[string]bool, len(sensitiveKeys))
	for _, key := range sensitiveKeys {
		sensitive[key] = true
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		if sensitive[key] {
			out[key] = RedactedPlaceholder
			continue
		}
		out[key] = value
	}
	return out
}
