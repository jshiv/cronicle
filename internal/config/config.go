package config

// Config is the configuration structure for the cronicle checker.
// https://raw.githubusercontent.com/mitchellh/golicense/master/config/config.go
type Config struct {
	Version  string `hcl:"version,optional"`
	Schedule Schedule
}

// Schedule is the configuration structure that defines a single cron job.
type Schedule struct {
	// Cron is the schedule interval. The field accepts standard cron
	// and other configurations listed here https://godoc.org/gopkg.in/robfig/cron.v2
	// i.e. ["@hourly", "@every 1h30m", "0 30 * * * *", "TZ=Asia/Tokyo 30 04 * * * *"]
	Cron    string `hcl:"cron,optional"`
	Command string `hcl:"command,optional"`
}
