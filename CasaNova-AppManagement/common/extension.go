package common

// ComposeExtension reads the compose extension metadata from a service or
// project's Extensions map. Prefers x-casanova (new canonical key), falls
// back to x-casaos (legacy CasaOS key) so existing app definitions work
// without modification.
func ComposeExtension(extensions map[string]interface{}) (interface{}, bool) {
	if v, ok := extensions[ComposeExtensionNameXCasaNova]; ok {
		return v, true
	}
	v, ok := extensions[ComposeExtensionNameXCasaOS]
	return v, ok
}
