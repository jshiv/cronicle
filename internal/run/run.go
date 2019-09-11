package run

import (
	"fmt"

	"github.com/jshiv/cronicle/internal/config"
	"github.com/jshiv/cronicle/internal/cron"
)

var conf config.Config
var schedule config.Schedule

// Run is the main function of the run package
func Run() {
	fmt.Println("run called")
	fmt.Println("This is something else.")
	conf.Version = "1.2.3"
	conf.Schedule.Cron = "@every 2s"
	conf.Schedule.Command = "echo Hello World"
	fmt.Println(conf)

	cron.RunSchedule(conf.Schedule)
}

func Dummy(in string) string {
	return in
}
