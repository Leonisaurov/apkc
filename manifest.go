package main

import (
	"encoding/xml"
	"os"
	"strconv"
	"strings"
)

type Manifest struct {
	XMLName     xml.Name    `xml:"manifest"`
	Package     string      `xml:"package,attr"`
	UsesSDK     UsesSDK     `xml:"uses-sdk"`
	Application Application `xml:"application"`
}

type UsesSDK struct {
	XMLName       xml.Name `xml:"uses-sdk"`
	MinSDKVersion string   `xml:"minSdkVersion,attr"`
}

// manifestMinAPI is the minSdkVersion of the project, or 0 when the manifest
// does not declare one. d8 needs it to decide how much desugaring to do, and
// without it it assumes API 1 and rewrites every lambda of every dependency.
func manifestMinAPI() int {
	mb, err := os.ReadFile("AndroidManifest.xml")
	if err != nil {
		return 0
	}

	manifest := Manifest{}
	if err := xml.Unmarshal(mb, &manifest); err != nil {
		return 0
	}

	api, err := strconv.Atoi(strings.TrimSpace(manifest.UsesSDK.MinSDKVersion))
	if err != nil || api < 1 {
		return 0
	}
	return api
}

// manifestChanged decides the manifest by content instead of by timestamp. The
// package name, the permissions and minSdkVersion all live in these bytes, and a
// copy that puts the same bytes back (a tool re-writing the merged manifest, a
// checkout, a cp -p restore) is not an edit: by timestamp it would throw away every
// compiled resource and every compiled source of the project for nothing. The caller
// stores the table only when this returns true, which is exactly when the digest
// below has to be kept.
func manifestChanged() bool {
	digest, err := fileSHA1("AndroidManifest.xml")
	if err != nil {
		return true
	}
	if digest == DataTable.ManifestDigest {
		return false
	}
	DataTable.ManifestDigest = digest
	return true
}

type Application struct {
	XMLName    xml.Name   `xml:"application"`
	Activities []Activity `xml:"activity"`
}

func (a Application) MainActivity() string {
	for _, a := range a.Activities {
		for _, i := range a.IntentFilter {
			for _, n := range i.Actions {
				if n.Name == "android.intent.action.MAIN" {
					return a.Name
				}
			}
		}
	}
	return ""
}

type Activity struct {
	XMLName      xml.Name       `xml:"activity"`
	Name         string         `xml:"name,attr"`
	IntentFilter []IntentFilter `xml:"intent-filter"`
}

type IntentFilter struct {
	XMLName xml.Name `xml:"intent-filter"`
	Actions []Action `xml:"action"`
}

type Action struct {
	XMLName xml.Name `xml:"action"`
	Name    string   `xml:"name,attr"`
}
