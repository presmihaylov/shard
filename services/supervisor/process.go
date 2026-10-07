package supervisor

// ProcessMode is shard-init's one argument for a process request on Linux: a run or a stop-process on stdin, the guest's answer on stdout.
const ProcessMode = "process"

// ProcessLogs is the directory under the shard mount where a container PID 1 writes each process's output.
const ProcessLogs = "logs"

// ProcessLogName is the file one process's output lands in, under ProcessLogs in a container and in a VM's state directory.
func ProcessLogName(name string) string { return name + ".log" }
