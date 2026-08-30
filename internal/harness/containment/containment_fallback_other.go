//go:build !linux && !darwin

package containment

func PrepareWithFallback(limit uint64) (Group, string, error) {
	group, err := Prepare(limit)
	if err != nil {
		return nil, "", err
	}
	return group, "kernel", nil
}
