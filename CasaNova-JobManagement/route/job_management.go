package route

import (
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-JobManagement/codegen"
	"github.com/labstack/echo/v4"
)

func (m *JobManagement) GetJobList(ctx echo.Context) error {
	return ctx.JSON(http.StatusNotImplemented, struct{}{})
}

func (m *JobManagement) CreateJob(ctx echo.Context) error {
	return ctx.JSON(http.StatusNotImplemented, struct{}{})
}

func (m *JobManagement) GetJob(ctx echo.Context, _ codegen.JobID) error {
	return ctx.JSON(http.StatusNotImplemented, struct{}{})
}

func (m *JobManagement) UpdateJobPriority(ctx echo.Context, _ codegen.JobID) error {
	return ctx.JSON(http.StatusNotImplemented, struct{}{})
}

func (m *JobManagement) GetJobStatus(ctx echo.Context, _ codegen.JobID) error {
	return ctx.JSON(http.StatusNotImplemented, struct{}{})
}

func (m *JobManagement) UpdateJobStatus(ctx echo.Context, _ codegen.JobID) error {
	return ctx.JSON(http.StatusNotImplemented, struct{}{})
}
