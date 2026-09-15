package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/common"
	interfaces "github.com/LeewayOpenSoftware/CasaNova-Common"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/systemctl"
)

const (
	appManagementConfigDirPath  = "/etc/casaos"
	appManagementConfigFilePath = "/etc/casaos/app-management.conf"
	appManagementName           = "casaos-app-management.service"
	appManagementNameShort      = "app-management"
)

//go:embedded ../../build/sysroot/etc/casaos/app-management.conf.sample
//var _appManagementConfigFileSample string

var (
	_logger *Logger
	// _status *version.GlobalMigrationStatus

	commit = "private build"
	date   = "private build"
)

func main() {
	versionFlag := flag.Bool("v", false, "version")
	debugFlag := flag.Bool("d", true, "debug")
	forceFlag := flag.Bool("f", false, "force")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("v%s\n", common.AppManagementVersion)
		os.Exit(0)
	}

	println("git commit:", commit)
	println("build date:", date)

	_logger = NewLogger()

	if os.Getuid() != 0 {
		_logger.Info("Root privileges are required to run this program.")
		os.Exit(1)
	}

	if *debugFlag {
		_logger.DebugMode = true
	}

	if !*forceFlag {
		isRunning, err := systemctl.IsServiceRunning(appManagementName)
		if err != nil {
			_logger.Error("Failed to check if %s is running", appManagementName)
			panic(err)
		}

		if isRunning {
			_logger.Info("%s is running. If migration is still needed, try with -f.", appManagementName)
			os.Exit(1)
		}
	}

	migrationTools := []interfaces.MigrationTool{
		NewMigration050(),          // CasaOS 0.4.x → CasaNova 0.5.0
		NewMigration0415AndOlder(), // Legacy URL migrations
	}

	var selectedMigrationTool interfaces.MigrationTool

	// look for the right migration tool matching current version
	//
	// The error used to be discarded. A tool that could not tell whether it was
	// needed answered false, and the run printed "No migration to proceed." and
	// exited 0 — indistinguishable, to an operator and to a packaging script,
	// from a box that was genuinely up to date. It is logged now; the tools
	// answer "needed" when they cannot check, because running an idempotent
	// migration again is the safe direction, but the reason has to be visible.
	for _, tool := range migrationTools {
		migrationNeeded, err := tool.IsMigrationNeeded()
		if err != nil {
			_logger.Error("could not determine whether a migration is needed, assuming it is: %s", err)
		}

		if migrationNeeded {
			selectedMigrationTool = tool
			break
		}
	}

	if selectedMigrationTool == nil {
		_logger.Info("No migration to proceed.")
		return
	}

	if err := selectedMigrationTool.PreMigrate(); err != nil {
		panic(err)
	}

	if err := selectedMigrationTool.Migrate(); err != nil {
		panic(err)
	}

	if err := selectedMigrationTool.PostMigrate(); err != nil {
		_logger.Error("Migration succeeded, but post-migration failed: %s", err)
	}
}
