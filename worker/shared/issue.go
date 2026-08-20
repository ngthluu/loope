package shared

type Label struct {
	Name string `json:"name"`
}

type Issue struct {
	Number int     `json:"number"`
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	Labels []Label `json:"labels"`
}

// HasLabel reports whether labels contains name (name == "" is never a match).
func HasLabel(labels []Label, name string) bool {
	if name == "" {
		return false
	}
	for _, l := range labels {
		if l.Name == name {
			return true
		}
	}
	return false
}
