package ktv

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Run starts ktv with the command line arguments (without the program name).
func Run(args []string) error {
	nsQuery, podQuery, err := parseArgs(args)
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(newModel(c, nsQuery, podQuery)).Run()
	return err
}

// parseArgs accepts "", "NS", "NS/POD" or "NS POD".
func parseArgs(args []string) (ns, pod string, err error) {
	switch len(args) {
	case 0:
		return "", "", nil
	case 1:
		ns, pod, _ = strings.Cut(args[0], "/")
		return ns, pod, nil
	case 2:
		return args[0], args[1], nil
	default:
		return "", "", fmt.Errorf("usage: ktv [NAMESPACE[/POD] | NAMESPACE POD]")
	}
}

// autoMatch returns the item matching query exactly, or else the only item
// containing it (case-insensitive).
func autoMatch(query string, items []string) (string, bool) {
	if query == "" {
		return "", false
	}
	var subs []string
	lower := strings.ToLower(query)
	for _, it := range items {
		if it == query {
			return it, true
		}
		if strings.Contains(strings.ToLower(it), lower) {
			subs = append(subs, it)
		}
	}
	if len(subs) == 1 {
		return subs[0], true
	}
	return "", false
}
