package app

import "fmt"

type ActorKind string

const (
	ActorProvider ActorKind = "PROVIDER"
	ActorService  ActorKind = "SERVICE"
	ActorBroker   ActorKind = "BROKER"
)

type Actor struct {
	Kind       ActorKind
	Subject    string
	ProviderID string
}

func ProviderActor(subject, providerID string) Actor {
	return Actor{Kind: ActorProvider, Subject: subject, ProviderID: providerID}
}

func ServiceActor(subject string) Actor {
	return Actor{Kind: ActorService, Subject: subject}
}

func BrokerActor(consumer string) Actor {
	return Actor{Kind: ActorBroker, Subject: consumer}
}

func (a Actor) CanActForProvider(providerID string) bool {
	switch a.Kind {
	case ActorProvider:
		return a.ProviderID != "" && a.ProviderID == providerID
	case ActorBroker, ActorService:
		return providerID != ""
	default:
		return false
	}
}

func (a Actor) CanReadProvider(providerID string) bool {
	return a.Kind == ActorService || (a.Kind == ActorProvider && a.ProviderID != "" && a.ProviderID == providerID)
}

func (a Actor) IsInternalService() bool {
	return a.Kind == ActorService
}

func (a Actor) RequireInternalService() error {
	if !a.IsInternalService() {
		return fmt.Errorf("%w: %s %q is not an internal service", ErrForbidden, a.Kind, a.Subject)
	}
	return nil
}
