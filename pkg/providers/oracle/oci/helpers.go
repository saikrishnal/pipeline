package oci

// Strings holds strings in an array
type Strings struct {
	strings []string
}

// NodePoolOptions holds node pool options as Strings
type NodePoolOptions struct {
	Images             Strings
	KubernetesVersions Strings
	Shapes             Strings
}

// Has checks if the strings array has a value
func (s Strings) Has(value string) bool {

	for _, v := range s.strings {
		if v == value {
			return true
		}
	}

	return false
}

// Get gets the raw array
func (s Strings) Get() []string {

	return s.strings
}
