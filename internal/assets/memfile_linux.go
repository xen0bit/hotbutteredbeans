package assets

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// memoryFile puts data in an anonymous memory file and returns a path the dynamic
// loader can open. The descriptor stays open for the life of the process.
func memoryFile(name string, data []byte) (string, error) {
	fd, err := unix.MemfdCreate(name, 0)
	if err != nil {
		return "", err
	}
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			unix.Close(fd)
			return "", err
		}
		data = data[n:]
	}
	return fmt.Sprintf("/proc/self/fd/%d", fd), nil
}
