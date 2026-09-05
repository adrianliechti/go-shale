// Linked into every embedded WASI command. WASI libc starts at "/" and has no
// way to seed its emulated working directory from the host, so this constructor
// runs before main and changes to the directory named by SHALE_CWD, which the Go
// host sets on every invocation. It replaces a per-project launcher patch.
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

__attribute__((constructor(200))) static void shale_cwd(void) {
	const char *cwd = getenv("SHALE_CWD");
	if (cwd == NULL || *cwd == '\0') {
		return;
	}
	if (chdir(cwd) != 0) {
		perror("cannot set working directory");
		_Exit(1);
	}
}
