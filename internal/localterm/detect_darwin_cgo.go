//go:build darwin && cgo

package localterm

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework Foundation
#import <AppKit/AppKit.h>
#include <stdlib.h>
#include <string.h>

// LaunchServices answers from its database: no permission prompt, nothing
// is started (experiment E11).
static char *cage_application_path(const char *bundle) {
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

func applicationPath(bundleID string) string {
	name := C.CString(bundleID)
	defer C.free(unsafe.Pointer(name))
	path := C.cage_application_path(name)
	if path != nil {
		defer C.free(unsafe.Pointer(path))
		if found := C.GoString(path); found != "" {
			if _, err := os.Stat(found); err == nil {
				return found
			}
		}
	}
	return fallbackBundlePath(bundleID)
}

// fallbackBundlePath checks well-known bundle locations directly so a freshly
// installed Herdr.app is detected immediately before LaunchServices indexes it.
func fallbackBundlePath(bundleID string) string {
	if bundleID != "dev.herdr.app" {
		return ""
	}
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, "Applications", "Herdr.app"))
	}
	candidates = append(candidates, "/Applications/Herdr.app")
	for _, candidate := range candidates {
		for _, marker := range []string{
			filepath.Join(candidate, "Contents", "MacOS", "Herdr"),
			filepath.Join(candidate, "Contents", "MacOS", "herdr-app"),
			filepath.Join(candidate, "Contents", "Info.plist"),
		} {
			if info, err := os.Stat(marker); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	return ""
}
