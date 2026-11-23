package config

import (
	"fmt"
	"strings"
)

// ParseAdapters parses "name=host:port,name=host:port" into name -> address.
func ParseAdapters(spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, entry := range strings.Split(spec, ",") {
		name, addr, ok := strings.Cut(entry, "=")
		if !ok || name == "" || addr == "" {
			return nil, fmt.Errorf("adapter entry %q: want name=host:port", entry)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("adapter %q listed twice", name)
		}
		out[name] = addr
	}
	return out, nil
}
