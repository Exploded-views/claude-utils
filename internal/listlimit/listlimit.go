// Package listlimit holds the one directory-size limit shared by the route
// walker and the budget harness, so the two cannot drift apart.
package listlimit

// Max is the most entries a directory may have to be listed in full. Under a
// bigger one the route walker lists nothing and resolves only a single name
// without "-".
const Max = 256
