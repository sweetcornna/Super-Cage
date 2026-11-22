package localterm

import "os"

// Windows: launches are not supported; the package only builds.
const openNoFollow = 0

func ownedByMe(os.FileInfo) bool { return true }
