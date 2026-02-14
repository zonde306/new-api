package common

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func GetEnvOrDefault(env string, defaultValue int) int {
	if env == "" || os.Getenv(env) == "" {
		return defaultValue
	}
	num, err := strconv.Atoi(os.Getenv(env))
	if err != nil {
		SysError(fmt.Sprintf("failed to parse %s: %s, using default value: %d", env, err.Error(), defaultValue))
		return defaultValue
	}
	return num
}

func GetEnvOrDefaultString(env string, defaultValue string) string {
	if env == "" || os.Getenv(env) == "" {
		return defaultValue
	}
	return os.Getenv(env)
}

func GetEnvOrDefaultBool(env string, defaultValue bool) bool {
	if env == "" || os.Getenv(env) == "" {
		return defaultValue
	}
	b, err := strconv.ParseBool(os.Getenv(env))
	if err != nil {
		SysError(fmt.Sprintf("failed to parse %s: %s, using default value: %t", env, err.Error(), defaultValue))
		return defaultValue
	}
	return b
}

// GetEnvOrDefaultDurationMS reads milliseconds from env and converts it to time.Duration.
// If the env value is <= 0, it falls back to defaultMs.
func GetEnvOrDefaultDurationMS(env string, defaultMs int) time.Duration {
	ms := GetEnvOrDefault(env, defaultMs)
	if ms <= 0 {
		ms = defaultMs
	}
	return time.Duration(ms) * time.Millisecond
}
