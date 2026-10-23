package vmruntime

func PrepareCoordinatorLock(string, int) error {
	return fault("helper_requires_linux")
}
