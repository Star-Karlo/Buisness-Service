// Package clients holds this service's outbound gRPC connections.
package clients

import (
	"context"
	"log/slog"
	"time"

	notificationv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/notification/v1"
	"github.com/karlo/business-service/internal/platform/grpcutil"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Notifier is the seam the business logic depends on.
//
// This interface is what replaces the 117 inline `Notification.create` calls
// the monolith had scattered through its order, agreement and shipment
// controllers. Business code states what happened and who should hear about it;
// it never names FCM, SMTP or WhatsApp, and it never builds message copy.
type Notifier interface {
	Notify(ctx context.Context, ev Event)
}

// Event is one thing that happened, worth telling someone about.
type Event struct {
	Type    notificationv1.EventType
	Subject Subject
	// Audience names the recipients. Resolving a role or company into a list of
	// users is the notification service's job.
	Audience *notificationv1.Audience
	Params   map[string]interface{}
	// ActorID is excluded from the audience, so nobody is notified of their own
	// action.
	ActorID string
	// IdempotencyKey makes a retry safe. Derive it from the subject and the
	// transition, not from the clock.
	IdempotencyKey string
}

// Subject identifies what the event is about, for the deep link.
type Subject struct {
	ID   string
	Type string
}

// Notification is the gRPC-backed Notifier.
type Notification struct {
	conn    *grpc.ClientConn
	client  notificationv1.NotificationServiceClient
	timeout time.Duration
}

// NewNotification dials the notification service.
func NewNotification(target, serviceName, serviceToken string, timeout time.Duration) (*Notification, error) {
	conn, err := grpcutil.Dial(grpcutil.DialConfig{
		Service:      serviceName,
		Target:       target,
		ServiceToken: serviceToken,
	})
	if err != nil {
		return nil, err
	}
	return &Notification{
		conn:    conn,
		client:  notificationv1.NewNotificationServiceClient(conn),
		timeout: timeout,
	}, nil
}

func (n *Notification) Close() error { return n.conn.Close() }

// Notify delivers an event, and never fails the caller.
//
// This is deliberate. An order that was successfully approved has been
// approved, whether or not the push notification about it went out. Propagating
// a notification error would roll back real business state because a
// best-effort side channel was briefly unavailable. Failures are logged, and
// the in-app record on the notification side is the durable copy.
func (n *Notification) Notify(ctx context.Context, ev Event) {
	params, err := structpb.NewStruct(ev.Params)
	if err != nil {
		slog.Error("notification params could not be encoded",
			"event", ev.Type.String(), "subject", ev.Subject.ID, "error", err)
		// Send without params rather than dropping the notification entirely:
		// the recipient still needs to know the thing happened.
		params = nil
	}

	// A fresh context, detached from the request. The caller's context may be
	// cancelled the moment their HTTP response is written, which would abort a
	// notification that was otherwise about to succeed.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), n.timeout)
	defer cancel()

	_, err = n.client.Notify(sendCtx, &notificationv1.NotifyRequest{
		Event:          ev.Type,
		Audience:       ev.Audience,
		SubjectId:      ev.Subject.ID,
		SubjectType:    ev.Subject.Type,
		Params:         params,
		ActorId:        ev.ActorID,
		IdempotencyKey: ev.IdempotencyKey,
	})
	if err != nil {
		slog.Error("notification delivery failed",
			"event", ev.Type.String(),
			"subject", ev.Subject.ID,
			"error", err,
		)
	}
}

// ---------------------------------------------------------------------------
// Audience constructors
// ---------------------------------------------------------------------------

// ToUsers addresses specific people.
func ToUsers(userIDs ...string) *notificationv1.Audience {
	return &notificationv1.Audience{
		Target: &notificationv1.Audience_Users{
			Users: &notificationv1.UserList{UserIds: userIDs},
		},
	}
}

// ToCompanyRoles addresses everyone in a company holding one of the roles.
// This is how "tell the transporter's dispatchers" is expressed without the
// business service having to fetch a user list.
func ToCompanyRoles(companyID string, roles ...string) *notificationv1.Audience {
	return &notificationv1.Audience{
		Target: &notificationv1.Audience_CompanyRole{
			CompanyRole: &notificationv1.CompanyRole{
				CompanyId: companyID,
				Roles:     roles,
			},
		},
	}
}

// ToTruckGroup addresses the drivers and managers of a fleet group.
func ToTruckGroup(truckGroupID string) *notificationv1.Audience {
	return &notificationv1.Audience{
		Target: &notificationv1.Audience_TruckGroup{
			TruckGroup: &notificationv1.TruckGroup{TruckGroupId: truckGroupID},
		},
	}
}

// NoopNotifier discards events. Used in tests, and as the fallback when the
// notification service is not configured, so business logic has no nil check.
type NoopNotifier struct{}

func (NoopNotifier) Notify(context.Context, Event) {}

// ToPhone addresses a raw number rather than a Karlo account.
//
// Needed for exactly one thing: the handover code at the unloading point. The
// receiving PIC is a warehouse employee of the customer, not a user of this
// system, so there is no account to notify — and requiring one would mean every
// delivery address had to be onboarded before a driver could hand over goods.
func ToPhone(number string) *notificationv1.Audience {
	return &notificationv1.Audience{
		Target: &notificationv1.Audience_Phone{
			Phone: &notificationv1.PhoneNumber{Number: number},
		},
	}
}
