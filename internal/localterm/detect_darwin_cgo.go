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

import "unsafe"

func applicationPath(bundleID string) string {
	name := C.CString(bundleID)
	defer C.free(unsafe.Pointer(name))
	path := C.cage_application_path(name)
	if path == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(path))
	return C.GoString(path)
}
