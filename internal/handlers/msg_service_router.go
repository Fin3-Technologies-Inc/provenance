package handlers

import (
	"context"
	"fmt"

	gogogrpc "github.com/gogo/protobuf/grpc"
	"github.com/gogo/protobuf/proto"
	"google.golang.org/grpc"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	"github.com/provenance-io/provenance/internal/antewrapper"
	msgfeeskeeper "github.com/provenance-io/provenance/x/msgfees/keeper"
)

// PioMsgServiceRouter routes fully-qualified Msg service methods to their handler with additional fee processing of msgs.
type PioMsgServiceRouter struct {
	interfaceRegistry codectypes.InterfaceRegistry
	routes            map[string]MsgServiceHandler
	msgFeesKeeper     msgfeeskeeper.Keeper
	decoder           sdk.TxDecoder
}

var _ gogogrpc.Server = &PioMsgServiceRouter{}

// NewPioMsgServiceRouter creates a new PioMsgServiceRouter.
func NewPioMsgServiceRouter(decoder sdk.TxDecoder) *PioMsgServiceRouter {
	return &PioMsgServiceRouter{
		routes:  map[string]MsgServiceHandler{},
		decoder: decoder,
	}
}

// MsgServiceHandler defines a function type which handles Msg service message.
type MsgServiceHandler = func(ctx sdk.Context, req sdk.Msg) (*sdk.Result, error)

// Handler returns the MsgServiceHandler for a given msg or nil if not found.
func (msr *PioMsgServiceRouter) Handler(msg sdk.Msg) MsgServiceHandler {
	return msr.routes[sdk.MsgTypeURL(msg)]
}

// HandlerByTypeURL returns the MsgServiceHandler for a given query route path or nil
// if not found.
func (msr *PioMsgServiceRouter) HandlerByTypeURL(typeURL string) MsgServiceHandler {
	return msr.routes[typeURL]
}

// SetMsgFeesKeeper sets the msg based fee keeper for retrieving msg fees.
func (msr *PioMsgServiceRouter) SetMsgFeesKeeper(msgFeesKeeper msgfeeskeeper.Keeper) {
	msr.msgFeesKeeper = msgFeesKeeper
}

// RegisterService implements the gRPC Server.RegisterService method. sd is a gRPC
// service description, handler is an object which implements that gRPC service.
//
// This function PANICs:
// - if it is called before the service `Msg`s have been registered using
//   RegisterInterfaces,
// - or if a service is being registered twice.
func (msr *PioMsgServiceRouter) RegisterService(sd *grpc.ServiceDesc, handler interface{}) {
	// Adds a top-level query handler based on the gRPC service name.
	for _, method := range sd.Methods {
		fqMethod := fmt.Sprintf("/%s/%s", sd.ServiceName, method.MethodName)
		methodHandler := method.Handler

		var requestTypeName string

		// NOTE: This is how we pull the concrete request type for each handler for registering in the InterfaceRegistry.
		// This approach is maybe a bit hacky, but less hacky than reflecting on the handler object itself.
		// We use a no-op interceptor to avoid actually calling into the handler itself.
		_, _ = methodHandler(nil, context.Background(), func(i interface{}) error {
			msg, ok := i.(sdk.Msg)
			if !ok {
				// We panic here because there is no other alternative and the app cannot be initialized correctly
				// this should only happen if there is a problem with code generation in which case the app won't
				// work correctly anyway.
				panic(fmt.Errorf("can't register request type %T for service method %s", i, fqMethod))
			}

			requestTypeName = sdk.MsgTypeURL(msg)
			return nil
		}, noopInterceptor)

		// Check that the service Msg fully-qualified method name has already
		// been registered (via RegisterInterfaces). If the user registers a
		// service without registering according service Msg type, there might be
		// some unexpected behavior down the road. Since we can't return an error
		// (`Server.RegisterService` interface restriction) we panic (at startup).
		reqType, err := msr.interfaceRegistry.Resolve(requestTypeName)
		if err != nil || reqType == nil {
			panic(
				fmt.Errorf(
					"type_url %s has not been registered yet. "+
						"Before calling RegisterService, you must register all interfaces by calling the `RegisterInterfaces` "+
						"method on module.BasicManager. Each module should call `msgservice.RegisterMsgServiceDesc` inside its "+
						"`RegisterInterfaces` method with the `_Msg_serviceDesc` generated by proto-gen",
					requestTypeName,
				),
			)
		}

		// Check that each service is only registered once. If a service is
		// registered more than once, then we should error. Since we can't
		// return an error (`Server.RegisterService` interface restriction) we
		// panic (at startup).
		_, found := msr.routes[requestTypeName]
		if found {
			panic(
				fmt.Errorf(
					"msg service %s has already been registered. Please make sure to only register each service once. "+
						"This usually means that there are conflicting modules registering the same msg service",
					fqMethod,
				),
			)
		}

		msr.routes[requestTypeName] = func(ctx sdk.Context, req sdk.Msg) (*sdk.Result, error) {
			msgTypeURL := sdk.MsgTypeURL(req)

			feeGasMeter, ok := ctx.GasMeter().(*antewrapper.FeeGasMeter)
			if !ok {
				panic("GasMeter is not of type FeeGasMeter")
			}

			tx, err := msr.decoder(ctx.TxBytes())
			if err != nil {
				panic(fmt.Errorf("error msg handling while getting txBytes: %w", err))
			}

			feeTx, ok := tx.(sdk.FeeTx)
			if feeTx == nil || !ok {
				panic("only Fee Tx are supported on provenance.")
			}

			fee, err := msr.msgFeesKeeper.GetMsgFee(ctx, msgTypeURL)
			if err != nil {
				return nil, err
			}
			if fee != nil && fee.AdditionalFee.IsPositive() {
				ctx.Logger().Debug(fmt.Sprintf("Tx Msg %v has an additional fee of %v ", msgTypeURL, fee.AdditionalFee))

				if !feeGasMeter.IsSimulate() {
					err = antewrapper.EnsureSufficientFees(runtimeGasForMsg(ctx), feeTx.GetFee(), feeGasMeter.FeeConsumed().Add(fee.AdditionalFee),
						msr.msgFeesKeeper.GetFloorGasPrice(ctx), msr.msgFeesKeeper.GetDefaultFeeDenom())
					if err != nil {
						return nil, err
					}
				}

				feeGasMeter.ConsumeFee(fee.AdditionalFee, msgTypeURL)
			}
			ctx = ctx.WithEventManager(sdk.NewEventManager())
			interceptor := func(goCtx context.Context, _ interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
				goCtx = context.WithValue(goCtx, sdk.SdkContextKey, ctx)
				return handler(goCtx, req)
			}
			// Call the method handler from the service description with the handler object.
			// We don't do any decoding here because the decoding was already done.
			res, err := methodHandler(handler, sdk.WrapSDKContext(ctx), noopDecoder, interceptor)
			if err != nil {
				return nil, err
			}

			resMsg, ok := res.(proto.Message)
			if !ok {
				return nil, sdkerrors.Wrapf(sdkerrors.ErrInvalidType, "Expecting proto.Message, got %T", resMsg)
			}

			return sdk.WrapServiceResult(ctx, resMsg, err)
		}
	}
}

// SetInterfaceRegistry sets the interface registry for the router.
func (msr *PioMsgServiceRouter) SetInterfaceRegistry(interfaceRegistry codectypes.InterfaceRegistry) {
	msr.interfaceRegistry = interfaceRegistry
}

func noopDecoder(_ interface{}) error { return nil }
func noopInterceptor(_ context.Context, _ interface{}, _ *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (interface{}, error) {
	return nil, nil
}

func runtimeGasForMsg(ctx sdk.Context) uint64 {
	return ctx.GasMeter().Limit()
}
