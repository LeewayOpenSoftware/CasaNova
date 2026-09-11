package route

import (
	"net/http"

	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/pkg/ysk"
	"github.com/labstack/echo/v4"
	"github.com/samber/lo"
)

func (r *APIRoute) DeleteYskCard(ctx echo.Context, id string) error {
	// A mutating bus endpoint, so it takes the bus's write gate like
	// PublishEvent and TriggerAction do. The deletion is by id alone with no
	// user scoping, and the cards are seeded server-wide — so one caller
	// removing a card removes it for EVERY user, which is shared state, not
	// a personal dismissal.
	if err := requireBusMutator(ctx); err != nil {
		return err
	}

	err := r.services.YSKService.DeleteYSKCard(ctx.Request().Context(), id)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: lo.ToPtr(err.Error()),
		})
	}
	return ctx.JSON(http.StatusOK, codegen.ResponseOK{
		Message: lo.ToPtr("success"),
	})
}

func (r *APIRoute) GetYskCard(ctx echo.Context) error {
	cardList, err := r.services.YSKService.YskCardList(ctx.Request().Context())
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, codegen.ResponseInternalServerError{
			Message: lo.ToPtr(err.Error()),
		})
	}

	return ctx.JSON(http.StatusOK, codegen.ResponseGetYSKCardListOK{
		Data: lo.ToPtr(lo.Map(cardList, func(yskCard ysk.YSKCard, _ int) codegen.YSKCard {
			card, err := ysk.ToCodegenYSKCard(yskCard)
			if err != nil {
				return codegen.YSKCard{}
			}
			return card
		})),
	})
}
