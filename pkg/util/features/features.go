package features

import (
	"context"
	"fmt"
	"maps"

	"github.com/devsy-org/admin-apis/pkg/licenseapi"
	stripe "github.com/stripe/stripe-go/v87"
)

const (
	metadataQueryFmt = "metadata['%s']:'%s'"
)

type SyncedFeature struct {
	Feature *stripe.EntitlementsFeature
	Product *stripe.Product
}

func EnsureFeatures(
	stripeClient *stripe.Client,
	syncedFeatures map[string]SyncedFeature,
	features []*licenseapi.Feature,
	additionalAttachAllProducts []stripe.Product,
	isLimit bool,
) error {
	err := EnsureStripeFeatures(stripeClient, syncedFeatures, features, isLimit)
	if err != nil {
		return err
	}

	if err = EnsureFeatureProducts(stripeClient, syncedFeatures); err != nil {
		return err
	}

	if !isLimit {
		if err = EnsureAttachAll(
			stripeClient,
			syncedFeatures,
			additionalAttachAllProducts,
		); err != nil {
			return err
		}
	}
	return nil
}

func EnsureStripeFeatures(
	stripeClient *stripe.Client,
	syncedFeatures map[string]SyncedFeature,
	features []*licenseapi.Feature,
	isLimit bool,
) error {
	for _, f := range features {
		err := EnsureFeatureExists(
			stripeClient,
			syncedFeatures,
			EnsureFeatureParams{
				Name:          f.Name,
				DisplayName:   f.DisplayName,
				ExtraMetadata: BuildMetadata(f, isLimit, nil),
			},
		)
		if err != nil {
			return err
		}

		if f.Preview {
			err = EnsureFeatureExists(
				stripeClient,
				syncedFeatures,
				EnsureFeatureParams{
					Name:        f.Name + "-preview",
					DisplayName: f.DisplayName + " [Preview]",
					ExtraMetadata: BuildMetadata(
						f,
						isLimit,
						map[string]string{
							licenseapi.MetadataKeyFeatureIsPreview: licenseapi.MetadataValueTrue,
						},
					),
				},
			)
			if err != nil {
				return err
			}
		}

		if isLimit {
			err = EnsureFeatureExists(
				stripeClient,
				syncedFeatures,
				EnsureFeatureParams{
					Name:        f.Name + "-active",
					DisplayName: f.DisplayName + " [Active]",
					ExtraMetadata: BuildMetadata(
						f,
						isLimit,
						map[string]string{
							licenseapi.MetadataKeyFeatureLimitTypeActive: licenseapi.MetadataValueTrue,
						},
					),
				},
			)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func BuildMetadata(
	feature *licenseapi.Feature,
	isLimit bool,
	extraMetadata map[string]string,
) map[string]string {
	metadata := map[string]string{}
	if isLimit {
		metadata[licenseapi.MetadataKeyFeatureIsLimit] = licenseapi.MetadataValueTrue
		feature.Name = licenseapi.LimitsPrefix + feature.Name
	}

	if feature.Status == string(licenseapi.FeatureStatusHidden) {
		metadata[licenseapi.MetadataKeyFeatureIsHidden] = licenseapi.MetadataValueTrue
	}

	for key, value := range extraMetadata {
		metadata[key] = value
	}

	return metadata
}

// EnsureFeatureParams holds parameters for EnsureFeatureExists.
type EnsureFeatureParams struct {
	Name          string
	DisplayName   string
	ExtraMetadata map[string]string
}

func EnsureFeatureExists(
	stripeClient *stripe.Client,
	syncedFeatures map[string]SyncedFeature,
	params EnsureFeatureParams,
) error {
	ctx := context.Background()
	name := params.Name
	displayName := params.DisplayName
	extraMetadata := params.ExtraMetadata
	feat, err := FindFeature(stripeClient, name)
	if err != nil {
		return err
	}

	if feat != nil {
		syncedFeatures[feat.Feature.ID] = *feat
		return nil
	}

	stripeParams := stripe.EntitlementsFeatureCreateParams{
		Name:      &displayName,
		LookupKey: &name,
	}

	for key, value := range extraMetadata {
		stripeParams.AddMetadata(key, value)
	}

	feature, err := stripeClient.V1EntitlementsFeatures.Create(ctx, &stripeParams)
	if err != nil {
		return fmt.Errorf(
			"failed to create Stripe feature from feature %s: %w",
			name,
			err,
		)
	}
	syncedFeatures[feature.ID] = SyncedFeature{
		Feature: feature,
	}
	return nil
}

func FindFeature(stripeClient *stripe.Client, id string) (*SyncedFeature, error) {
	ctx := context.Background()
	for feature, err := range stripeClient.V1EntitlementsFeatures.List(ctx, &stripe.EntitlementsFeatureListParams{
		LookupKey: &id,
	}).All(ctx) {
		if err != nil {
			return nil, fmt.Errorf(
				"failed to list features while check if feature [%s] exists: %w",
				id,
				err,
			)
		}
		return &SyncedFeature{Feature: feature}, nil
	}
	return nil, nil
}

func EnsureFeatureProducts(
	stripeClient *stripe.Client,
	syncedFeatures map[string]SyncedFeature,
) error {
	for key, feature := range syncedFeatures {
		if feature.Product != nil {
			continue
		}

		product, err := EnsureFeatureProduct(stripeClient, &feature)
		if err != nil {
			return err
		}
		feature.Product = product
		syncedFeatures[key] = feature
	}
	return nil
}

func EnsureFeatureProduct(
	stripeClient *stripe.Client,
	syncedFeature *SyncedFeature,
) (*stripe.Product, error) {
	ctx := context.Background()
	for product, err := range stripeClient.V1Products.Search(ctx, &stripe.ProductSearchParams{
		SearchParams: stripe.SearchParams{
			Query: fmt.Sprintf(
				metadataQueryFmt,
				licenseapi.MetadataKeyProductForFeature,
				syncedFeature.Feature.LookupKey,
			),
		},
	}).All(ctx) {
		if err != nil {
			return nil, err
		}
		// a product exists with features name
		return product, nil
	}

	usdCurrencyCode := "usd"
	unit := int64(2000000) // sample placeholder price
	interval := "year"
	intervalCount := int64(1)
	product, err := stripeClient.V1Products.Create(ctx, &stripe.ProductCreateParams{
		Name: &syncedFeature.Feature.Name,
		DefaultPriceData: &stripe.ProductCreateDefaultPriceDataParams{
			Currency:   &usdCurrencyCode,
			UnitAmount: &unit,
			Recurring: &stripe.ProductCreateDefaultPriceDataRecurringParams{
				Interval:      &interval,
				IntervalCount: &intervalCount,
			},
		},
		Metadata: map[string]string{
			licenseapi.MetadataKeyProductForFeature: syncedFeature.Feature.LookupKey,
		},
	})
	if err != nil {
		return nil, err
	}

	_, err = stripeClient.V1ProductFeatures.Create(ctx, &stripe.ProductFeatureCreateParams{
		Product:            &product.ID,
		EntitlementFeature: &syncedFeature.Feature.ID,
	})
	if err != nil {
		return nil, err
	}
	return product, nil
}

func EnsureAttachAll(
	stripeClient *stripe.Client,
	features map[string]SyncedFeature,
	attachTo []stripe.Product,
) error {
	ctx := context.Background()
	if attachTo == nil {
		attachTo = []stripe.Product{}
	}
	for prod, err := range stripeClient.V1Products.Search(ctx, &stripe.ProductSearchParams{
		SearchParams: stripe.SearchParams{
			Query: fmt.Sprintf(
				metadataQueryFmt,
				licenseapi.MetadataKeyAttachAll,
				licenseapi.MetadataValueTrue,
			),
		},
	}).All(ctx) {
		if err != nil {
			return err
		}
		attachTo = append(attachTo, *prod)
	}

	prodIDs := map[string]bool{}

	for _, prod := range attachTo {
		alreadyDone, ok := prodIDs[prod.ID]
		if ok && alreadyDone {
			continue
		}
		prodIDs[prod.ID] = true

		featuresToCheck := maps.Clone(features)

		if err := SearchProductForFeatures(stripeClient, prod.ID, featuresToCheck); err != nil {
			return err
		}
		for featureID, feat := range featuresToCheck {
			metadataHiddenValue, ok := feat.Feature.Metadata[licenseapi.MetadataKeyFeatureIsHidden]
			if !ok || metadataHiddenValue != licenseapi.MetadataValueTrue {
				_, err := stripeClient.V1ProductFeatures.Create(
					ctx,
					&stripe.ProductFeatureCreateParams{
						Product:            &prod.ID,
						EntitlementFeature: &featureID,
					},
				)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func SearchProductForFeatures(
	stripeClient *stripe.Client,
	productID string,
	featuresToCheck map[string]SyncedFeature,
) error {
	ctx := context.Background()
	for productFeature, err := range stripeClient.V1ProductFeatures.List(ctx, &stripe.ProductFeatureListParams{
		Product: &productID,
	}).All(ctx) {
		if err != nil {
			return err
		}
		delete(featuresToCheck, productFeature.EntitlementFeature.ID)
	}
	return nil
}
