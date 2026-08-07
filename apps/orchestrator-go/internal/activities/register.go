package activities

import "go.temporal.io/sdk/activity"

// RegisterOptions returns the ActivityOptions used to register an activity
// under a stable name across both the production worker and the test
// environments. Keeping the name centralised avoids string drift.
func RegisterOptions(name string) activity.RegisterOptions {
	return activity.RegisterOptions{Name: name}
}
