package jobs

import "time"

// Transient permits retries only after target evidence has established the outcome.
func Transient(code string) bool {
	switch code {
	case "40001", "40P01", "55P03", "53300", "57P01", "57P02", "57P03",
		"08000", "08001", "08003", "08004", "08006", "08007", "08P01",
		"target_unavailable", "network_error", "worker_stopped", "worker_interrupted":
		return true
	}
	return false
}

func RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 5 {
		return 30 * time.Second
	}
	return time.Second * time.Duration(1<<uint(attempt-1))
}
