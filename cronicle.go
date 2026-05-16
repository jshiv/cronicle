/*
Copyright © 2019 NAME HERE <EMAIL ADDRESS>

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package main

import (
	"github.com/jshiv/cronicle/cmd"

	// Embed the IANA tz database into the binary. Without this, schedules
	// like `timezone = "America/Los_Angeles"` fail to load on any system
	// without tzdata installed (notably minimal alpine containers), and
	// the config reload aborts — keeping the previous schedule set in
	// memory and silently dropping newly-PUT schedules. Adds ~450KB to
	// the binary in exchange for making it self-contained.
	_ "time/tzdata"
)

func main() {
	cmd.Execute()
}
