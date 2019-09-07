
package config


// Config is the configuration structure for the license checker.
// https://raw.githubusercontent.com/mitchellh/golicense/master/config/config.go

type Config struct {
	// Allow and Deny are the list of licenses that are allowed or disallowed,
	// respectively. The string value here can be either the license name
	// (case insensitive) or the SPDX ID (case insensitive).
	//
	// If a license is found that isn't in either list, then a warning is
	// emitted. If a license is in both deny and allow, then deny takes
	// priority.
	Allow []string `hcl:"allow,optional"`
	Deny  []string `hcl:"deny,optional"`

	// Override is a map that explicitly sets the license for the given
	// import path. The key is an import path (exact) and the value is
	// the name or SPDX ID of the license. Regardless, the value will
	// be set as both the name and SPDX ID, so SPDX IDs are recommended.
	Override map[string]string `hcl:"override,optional"`

	// Translate is a map that translates one import source into another.
	// For example, "gopkg.in/(.*)" => "github.com/\1" would translate
	// gopkg into github (incorrectly, but the example would work).
	Translate map[string]string `hcl:"translate,optional"`
}
