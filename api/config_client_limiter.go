package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-gost/x/config"
	parser "github.com/go-gost/x/config/parsing/limiter"
	"github.com/go-gost/x/registry"
)

// swagger:parameters getClientLimiterListRequest
type getClientLimiterListRequest struct {
}

// successful operation.
// swagger:response getClientLimiterListResponse
type getClientLimiterListResponse struct {
	// in: body
	Data clientLimiterList
}

type clientLimiterList struct {
	Count int                     `json:"count"`
	List  []*config.LimiterConfig `json:"list"`
}

func getClientLimiterList(ctx *gin.Context) {
	// swagger:route GET /config/ilimiters Limiter getClientLimiterListRequest
	//
	// Get client limiter list.
	//
	//     Security:
	//       basicAuth: []
	//
	//     Responses:
	//       200: getClientLimiterListResponse

	var req getClientLimiterListRequest
	ctx.ShouldBindQuery(&req)

	list := config.Global().ILimiters

	var resp getClientLimiterListResponse
	resp.Data = clientLimiterList{
		Count: len(list),
		List:  list,
	}

	ctx.JSON(http.StatusOK, Response{
		Data: resp.Data,
	})
}

// swagger:parameters getClientLimiterRequest
type getClientLimiterRequest struct {
	// in: path
	// required: true
	Limiter string `uri:"limiter" json:"limiter"`
}

// successful operation.
// swagger:response getClientLimiterResponse
type getClientLimiterResponse struct {
	// in: body
	Data *config.LimiterConfig
}

func getClientLimiter(ctx *gin.Context) {
	// swagger:route GET /config/ilimiters/{limiter} Limiter getClientLimiterRequest
	//
	// Get client limiter.
	//
	//     Security:
	//       basicAuth: []
	//
	//     Responses:
	//       200: getClientLimiterResponse

	var req getClientLimiterRequest
	ctx.ShouldBindUri(&req)

	var resp getClientLimiterResponse

	for _, limiter := range config.Global().ILimiters {
		if limiter == nil {
			continue
		}
		if limiter.Name == req.Limiter {
			resp.Data = limiter
		}
	}

	ctx.JSON(http.StatusOK, Response{
		Data: resp.Data,
	})
}

// swagger:parameters createClientLimiterRequest
type createClientLimiterRequest struct {
	// in: body
	Data config.LimiterConfig `json:"data"`
}

// successful operation.
// swagger:response createClientLimiterResponse
type createClientLimiterResponse struct {
	Data Response
}

func createClientLimiter(ctx *gin.Context) {
	// swagger:route POST /config/ilimiters Limiter createClientLimiterRequest
	//
	// Create a new client limiter, the name of limiter must be unique in limiter list.
	//
	//     Security:
	//       basicAuth: []
	//
	//     Responses:
	//       200: createClientLimiterResponse

	var req createClientLimiterRequest
	ctx.ShouldBindJSON(&req.Data)

	name := strings.TrimSpace(req.Data.Name)
	if name == "" {
		writeError(ctx, NewError(http.StatusBadRequest, ErrCodeInvalid, "limiter name is required"))
		return
	}
	req.Data.Name = name

	if registry.ClientLimiterRegistry().IsRegistered(name) {
		writeError(ctx, NewError(http.StatusBadRequest, ErrCodeDup, fmt.Sprintf("limiter %s already exists", name)))
		return
	}

	v := parser.ParseClientLimiter(&req.Data)
	if err := registry.ClientLimiterRegistry().Register(name, v); err != nil {
		writeError(ctx, NewError(http.StatusInternalServerError, ErrCodeFailed, fmt.Sprintf("create limiter %s failed: %s", name, err.Error())))
		return
	}

	config.OnUpdate(func(c *config.Config) error {
		c.ILimiters = append(c.ILimiters, &req.Data)
		return nil
	})

	ctx.JSON(http.StatusOK, Response{
		Msg: "OK",
	})
}

// swagger:parameters updateClientLimiterRequest
type updateClientLimiterRequest struct {
	// in: path
	// required: true
	Limiter string `uri:"limiter" json:"limiter"`
	// in: body
	Data config.LimiterConfig `json:"data"`
}

// successful operation.
// swagger:response updateClientLimiterResponse
type updateClientLimiterResponse struct {
	Data Response
}

func updateClientLimiter(ctx *gin.Context) {
	// swagger:route PUT /config/ilimiters/{limiter} Limiter updateClientLimiterRequest
	//
	// Update client limiter by name, the limiter must already exist.
	//
	//     Security:
	//       basicAuth: []
	//
	//     Responses:
	//       200: updateClientLimiterResponse

	var req updateClientLimiterRequest
	ctx.ShouldBindUri(&req)
	ctx.ShouldBindJSON(&req.Data)

	name := strings.TrimSpace(req.Limiter)

	if !registry.ClientLimiterRegistry().IsRegistered(name) {
		writeError(ctx, NewError(http.StatusBadRequest, ErrCodeNotFound, fmt.Sprintf("limiter %s not found", name)))
		return
	}

	req.Data.Name = name

	v := parser.ParseClientLimiter(&req.Data)

	registry.ClientLimiterRegistry().Unregister(name)

	if err := registry.ClientLimiterRegistry().Register(name, v); err != nil {
		writeError(ctx, NewError(http.StatusBadRequest, ErrCodeDup, fmt.Sprintf("limiter %s already exists", name)))
		return
	}

	config.OnUpdate(func(c *config.Config) error {
		for i := range c.ILimiters {
			if c.ILimiters[i].Name == name {
				c.ILimiters[i] = &req.Data
				break
			}
		}
		return nil
	})

	ctx.JSON(http.StatusOK, Response{
		Msg: "OK",
	})
}

// swagger:parameters deleteClientLimiterRequest
type deleteClientLimiterRequest struct {
	// in: path
	// required: true
	Limiter string `uri:"limiter" json:"limiter"`
}

// successful operation.
// swagger:response deleteClientLimiterResponse
type deleteClientLimiterResponse struct {
	Data Response
}

func deleteClientLimiter(ctx *gin.Context) {
	// swagger:route DELETE /config/ilimiters/{limiter} Limiter deleteClientLimiterRequest
	//
	// Delete client limiter by name.
	//
	//     Security:
	//       basicAuth: []
	//
	//     Responses:
	//       200: deleteClientLimiterResponse

	var req deleteClientLimiterRequest
	ctx.ShouldBindUri(&req)

	name := strings.TrimSpace(req.Limiter)

	if !registry.ClientLimiterRegistry().IsRegistered(name) {
		writeError(ctx, NewError(http.StatusBadRequest, ErrCodeNotFound, fmt.Sprintf("limiter %s not found", name)))
		return
	}
	registry.ClientLimiterRegistry().Unregister(name)

	config.OnUpdate(func(c *config.Config) error {
		limiters := c.ILimiters
		c.ILimiters = nil
		for _, s := range limiters {
			if s.Name == name {
				continue
			}
			c.ILimiters = append(c.ILimiters, s)
		}
		return nil
	})

	ctx.JSON(http.StatusOK, Response{
		Msg: "OK",
	})
}
