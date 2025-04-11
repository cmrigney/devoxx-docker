package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"

	"github.com/rumpl/devoxx-docker/remote"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "child" {
		err := child()
		if err != nil {
			log.Fatalln("Error in child process:", err)
		}
		return
	}

	if len(os.Args) >= 2 && os.Args[1] == "pull" {
		err := pull()
		if err != nil {
			log.Fatalln("Error in pull process:", err)
		}
		return
	}

	// Must be: ./binary run <image> <command> <args>
	if len(os.Args) >= 4 && os.Args[1] == "run" {
		err := run()
		if err != nil {
			log.Fatalln("Error in parent process:", err)
		}
		return
	}

	log.Fatalln("Usage: ./devoxx-container [pull|run]")
}

func run() error {
	logger := log.New(os.Stdout, "PARENT: ", 0)

	pid := os.Getpid()
	logger.Printf("Parent process PID: %d\n", pid)

	err := setupCgroup()
	if err != nil {
		return err
	}

	image := os.Args[2]

	err = setupVolume("/fs/" + image + "/rootfs/my-volume")
	if err != nil {
		return fmt.Errorf("failed to setup volume: %w", err)
	}

	cmd := exec.Command("/proc/self/exe", append([]string{"child"}, os.Args[2:]...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS | syscall.CLONE_NEWNET,
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	err = writeProcToCgroup(cmd.Process.Pid)
	if err != nil {
		return err
	}

	err = setupVeth("veth", cmd.Process.Pid)
	if err != nil {
		return fmt.Errorf("failed to setup veth: %w", err)
	}
	defer cleanupVeth("veth")

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("command finished with error: %w", err)
	}

	logger.Printf("Container exited with exit code %d", cmd.ProcessState.ExitCode())

	return nil
}

// Args: ./binary child <image> <command> <args>
func child() error {
	logger := log.New(os.Stdout, "CHILD: ", 0)

	image := os.Args[2]

	err := mountVolume("volume", "/fs/"+image+"/rootfs/my-volume")
	if err != nil {
		return fmt.Errorf("failed to mount volume: %w", err)
	}
	defer unmountVolume("/fs/" + image + "/rootfs/my-volume")

	err = setupContainer(logger, image)
	if err != nil {
		return fmt.Errorf("failed to setup container: %w", err)
	}

	err = setupContainerNetworking("veth")
	if err != nil {
		return fmt.Errorf("failed to setup container networking: %w", err)
	}

	err = syscall.Sethostname([]byte("my-container"))
	if err != nil {
		return fmt.Errorf("failed to set hostname: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %w", err)
	}

	logger.Printf("Hostname: %s\n", hostname)

	cmd := exec.Command(os.Args[3], os.Args[4:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func pull() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("no image name provided")
	}

	image := os.Args[2]

	fmt.Printf("Pulling image: %s\n", image)

	puller := remote.NewImagePuller(image)
	err := puller.Pull()
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	fmt.Println("Pulling done")

	return nil
}

func setupContainer(logger *log.Logger, image string) error {
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %w", err)
	}

	logger.Printf("Current working directory: %s\n", wd)

	// Could use pivot_root instead
	err = syscall.Chroot("/fs/" + image + "/rootfs")
	if err != nil {
		return fmt.Errorf("failed to change root directory: %w", err)
	}

	err = os.Chdir("/")
	if err != nil {
		return fmt.Errorf("failed to change directory: %w", err)
	}

	err = syscall.Mount("proc", "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, "")
	if err != nil {
		return fmt.Errorf("failed to mount proc filesystem: %w", err)
	}

	return nil
}

func setupCgroup() error {
	err := os.Mkdir("/sys/fs/cgroup/devoxx", 0755)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create cgroup directory: %w", err)
	}

	memLimit := 1024 * 1024 * 100 // 100MB
	err = os.WriteFile("/sys/fs/cgroup/devoxx/memory.max", fmt.Appendf(nil, "%d", memLimit), 0644)
	if err != nil {
		return fmt.Errorf("failed to set memory limit: %w", err)
	}

	cpuPeriod := 100 * 1000 // 100ms
	cpuQuota := 50 * 1000   // 50ms
	err = os.WriteFile("/sys/fs/cgroup/devoxx/cpu.max", fmt.Appendf(nil, "%d %d", cpuQuota, cpuPeriod), 0644)
	if err != nil {
		return fmt.Errorf("failed to set cpu max: %w", err)
	}

	return nil
}

func writeProcToCgroup(pid int) error {
	err := os.WriteFile("/sys/fs/cgroup/devoxx/cgroup.procs", fmt.Appendf(nil, "%d", pid), 0644)
	if err != nil {
		return fmt.Errorf("failed to add process to cgroup: %w", err)
	}

	return nil
}

func setupVolume(containerPath string) error {
	err := os.MkdirAll(containerPath, 0755)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create container path: %w", err)
	}

	return nil
}

func mountVolume(source, target string) error {
	err := syscall.Mount(source, target, "", syscall.MS_PRIVATE|syscall.MS_BIND, "")
	if err != nil {
		return fmt.Errorf("failed to mount volume: %w", err)
	}
	return nil
}

func unmountVolume(target string) error {
	err := syscall.Unmount(target, syscall.MNT_FORCE)
	if err != nil {
		return fmt.Errorf("failed to unmount volume: %w", err)
	}
	err = os.RemoveAll(target) // TODO this doesn't seem to delete the directory? Maybe it's still unmounting?
	if err != nil {
		return fmt.Errorf("failed to remove volume directory: %w", err)
	}

	return nil
}

func setupVeth(vethName string, pid int) error {
	err := exec.Command("ip", "link", "add", vethName+"0", "type", "veth", "peer", "name", vethName+"1").Run()
	if err != nil {
		return fmt.Errorf("failed to create veth pair: %w", err)
	}

	err = exec.Command("ip", "link", "set", vethName+"1", "netns", fmt.Sprintf("%d", pid)).Run()
	if err != nil {
		return fmt.Errorf("failed to move veth to container namespace: %w", err)
	}

	err = exec.Command("ip", "addr", "add", "10.0.0.1/24", "dev", vethName+"0").Run()
	if err != nil {
		return fmt.Errorf("failed to assign IP address to veth: %w", err)
	}

	err = exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "10.0.0.0/24", "-j", "MASQUERADE").Run()
	if err != nil {
		return fmt.Errorf("failed to set up NAT: %w", err)
	}

	return nil
}

func cleanupVeth(vethName string) error {
	err := exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", "10.0.0.0/24", "-j", "MASQUERADE").Run()
	if err != nil {
		return fmt.Errorf("failed to set up NAT: %w", err)
	}

	err = exec.Command("ip", "link", "delete", vethName+"0").Run()
	if err != nil {
		return fmt.Errorf("failed to delete veth pair: %w", err)
	}

	return nil
}

func setupContainerNetworking(vethName string) error {
	err := exec.Command("ip", "addr", "add", "10.0.0.25/24", "dev", vethName+"1").Run()
	if err != nil {
		return fmt.Errorf("failed to assign IP address to veth: %w", err)
	}

	err = exec.Command("ip", "link", "set", vethName+"1", "up").Run()
	if err != nil {
		return fmt.Errorf("failed to set veth link up: %w", err)
	}

	err = exec.Command("ip", "link", "set", "lo", "up").Run()
	if err != nil {
		return fmt.Errorf("failed to set loopback link up: %w", err)
	}

	err = exec.Command("ip", "route", "add", "default", "via", "10.0.0.1").Run()
	if err != nil {
		return fmt.Errorf("failed to set default route: %w", err)
	}

	return nil
}
