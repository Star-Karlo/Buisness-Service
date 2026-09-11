package services

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/fieldconfig"
	"github.com/karlo/business-service/internal/repository"
)

// FieldConfigService answers "what does this company's agreement form look
// like" and "may this company change that".
type FieldConfigService struct {
	repo *repository.FieldConfigRepository
}

func NewFieldConfigService(repo *repository.FieldConfigRepository) *FieldConfigService {
	return &FieldConfigService{repo: repo}
}

// For returns the effective configuration for a company and entity.
//
// Callers use it for two different purposes and both matter: a form renderer
// asks what to draw, and a create handler asks what to demand. They must be the
// same answer, which is why there is one method rather than a form endpoint and
// a separate validation path that can drift apart.
func (s *FieldConfigService) For(ctx context.Context, companyID uuid.UUID, entity fieldconfig.Entity) (fieldconfig.Config, error) {
	if _, ok := fieldconfig.Catalog[entity]; !ok {
		return fieldconfig.Config{}, fmt.Errorf("%w: no configurable entity %q", ErrValidation, entity)
	}

	overrides, err := s.repo.Overrides(ctx, companyID, entity)
	if err != nil {
		// A configuration read that fails must not fall back to defaults. A
		// company that hides a field would suddenly be shown it, and — worse —
		// a company that requires one would stop demanding it, so orders would
		// be accepted without detail their operation depends on. Failing is
		// the safe answer.
		return fieldconfig.Config{}, err
	}

	return fieldconfig.Resolve(entity, overrides), nil
}

// FieldSetting is one requested change.
type FieldSetting struct {
	Key           string                  `json:"key"`
	Requirement   fieldconfig.Requirement `json:"requirement"`
	LabelOverride *string                 `json:"labelOverride"`
}

// Update applies a company administrator's changes.
//
// Every setting is checked before any is written. A partial application would
// leave the form in a state nobody asked for — half the kecamatan fields
// enabled, say — which is harder to reason about than a refusal.
func (s *FieldConfigService) Update(ctx context.Context, actor Actor, entity fieldconfig.Entity, settings []FieldSetting) (fieldconfig.Config, error) {
	for _, set := range settings {
		if err := fieldconfig.CheckOverride(entity, set.Key, set.Requirement); err != nil {
			return fieldconfig.Config{}, fmt.Errorf("%w: %v", ErrValidation, err)
		}
	}

	for _, set := range settings {
		if err := s.repo.SetOverride(ctx, actor.CompanyID, entity, set.Key,
			set.Requirement, set.LabelOverride, actor.UserID); err != nil {
			return fieldconfig.Config{}, err
		}
	}

	return s.For(ctx, actor.CompanyID, entity)
}

// Validate checks a submission against the company's configuration.
//
// present names the keys the caller actually supplied, which the handler builds
// from the request body rather than from the bound struct: a quantity of zero
// and an omitted quantity are different submissions, and a zero-value check
// cannot tell them apart.
func (s *FieldConfigService) Validate(ctx context.Context, companyID uuid.UUID, entity fieldconfig.Entity, present map[string]bool) error {
	cfg, err := s.For(ctx, companyID, entity)
	if err != nil {
		return err
	}
	if err := cfg.Validate(present); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return nil
}
