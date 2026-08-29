package main

import (
	"fmt"
	"os"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/cmd/validator/pkg"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	utils_logger "github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
)

var logger = NewLogger()

func main() {
	utils_logger.LogInitConsoleOnly()

	if len(os.Args) < 1 {
		os.Args = append(os.Args, "-h")
	}

	dockerComposeFilePath := os.Args[1]

	// check file exists
	if _, err := os.Stat(dockerComposeFilePath); os.IsNotExist(err) {
		logger.Error("docker-compose file does not exist: %s", dockerComposeFilePath)
		os.Exit(1)
	}

	composeFileContent := file.ReadFullFile(dockerComposeFilePath)

	err := pkg.VaildDockerCompose(composeFileContent)
	if err != nil {
		logger.Error("failed to parse docker-compose file %s", err.Error())
		os.Exit(1)
	}
	fmt.Println("pass validate")
}
