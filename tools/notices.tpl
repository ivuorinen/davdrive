Third-party notices for davdrive

davdrive release binaries include the packages below. Each is listed with its
licence, followed by the licence text its authors ship. davdrive's own licence
is in LICENSE.
{{ range . }}
================================================================================
{{ .Name }}
Licence: {{ .LicenseName }}{{ if ne .LicenseURL "Unknown" }}
Source:  {{ .LicenseURL }}{{ end }}{{ if eq .Name "github.com/willscott/go-nfs" }}
Note:    modified for davdrive; see third_party/go-nfs-UPSTREAM.md in its source.{{ end }}
--------------------------------------------------------------------------------
{{ .LicenseText }}
{{ end }}
