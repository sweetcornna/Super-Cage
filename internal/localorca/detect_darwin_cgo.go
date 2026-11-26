//go:build darwin && cgo

package localorca

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework Foundation
#import <AppKit/AppKit.h>
#include <stdlib.h>
#include <string.h>

static char *cage_orca_application_path(const char *bundle) {
	@autoreleasepool {
		NSString *identifier = [NSString stringWithUTF8String:bundle];
		NSURL *url = [[NSWorkspace sharedWorkspace] URLForApplicationWithBundleIdentifier:identifier];
		if (url == nil || url.path == nil) {
			return NULL;
		}
		return strdup(url.path.fileSystemRepresentation);
	}
}
*/
import "C"

import (
	"os"
	"path/filepath"
	"unsafe"
)

const macOSBundleID = "com.stablyai.orca"

func detectLocal() Installation {
	appPath := applicationPath(macOSBundleID)
	if appPath == "" {
		appPath = fallbackMacOSAppPath()
	}
	if appPath == "" {
		return Installation{}
	}
	cliPath := filepath.Join(appPath, "Contents", "Resources", "bin", "orca")
	if info, err := os.Stat(cliPath); err != nil || info.IsDir() {
		cliPath = ""
	}
	ver := readBuildVersion(filepath.Join(appPath, "Contents", "Resources", "orca-local-build.json"))
	return Installation{
		Installed: cliPath != "" || appPath != "",
		AppPath:   appPath,
		CLIPath:   cliPath,
		Version:   ver,
	}
}

func applicationPath(bundleID string) string {
	name := C.CString(bundleID)
	defer C.free(unsafe.Pointer(name))
	path := C.cage_orca_application_path(name)
	if path == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(path))
	found := C.GoString(path)
	if found == "" {
		return ""
	}
	if _, err := os.Stat(found); err != nil {
		return ""
	}
	return found
}

func fallbackMacOSAppPath() string {
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, "Applications", "Orca.app"))
	}
	candidates = append(candidates, "/Applications/Orca.app")
	for _, candidate := range candidates {
		if info, err := os.Stat(filepath.Join(candidate, "Contents", "Info.plist")); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}
