package cronicle

import (
	"time"

	log "github.com/sirupsen/logrus"
)

//TZFormatter enables timezone specifc logrus formatting
// Example:
// loc, _ = time.LoadLocation("America/Los_Angeles")
// log.SetFormatter(TZFormatter{Formatter: &log.TextFormatter{
// 	FullTimestamp: true,
// 	}, loc: loc})
type TZFormatter struct {
	log.Formatter
	loc *time.Location
}

//Format sets the timezone for the given loc *time.Timezone
func (u TZFormatter) Format(e *log.Entry) ([]byte, error) {
	e.Time = e.Time.In(u.loc)
	return u.Formatter.Format(e)
}
