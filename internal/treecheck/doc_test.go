// Package treecheck holds the tests that check the whole checkout: the
// conventions every Go file keeps, and what the committed references may
// carry. They read the tree through reporoot and link none of the code they
// check, so a change anywhere in the tree reruns this package alone, not
// the package a check would otherwise sit in.
package treecheck
