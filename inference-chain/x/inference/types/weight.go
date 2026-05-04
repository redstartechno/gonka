package types

import mathsdk "cosmossdk.io/math"

func ConfirmationWeightOfParticipant(p *ActiveParticipant, scales []*ConfirmationWeightScale) int64 {
	if p == nil {
		return 0
	}
	modelNodes := make(map[string][]*MLNodeInfo, len(p.Models))
	for i, modelID := range p.Models {
		if modelID == "" || i >= len(p.MlNodes) || p.MlNodes[i] == nil {
			continue
		}
		modelNodes[modelID] = append(modelNodes[modelID], p.MlNodes[i].MlNodes...)
	}
	return confirmationWeight(modelNodes, scales)
}

func ConfirmationWeightOfModelNodes(modelNodes map[string][]*MLNodeInfo, scales []*ConfirmationWeightScale) int64 {
	return confirmationWeight(modelNodes, scales)
}

func confirmationWeight(modelNodes map[string][]*MLNodeInfo, scales []*ConfirmationWeightScale) int64 {
	coefficients := confirmationCoefficients(scales)
	total := int64(0)
	for modelID, nodes := range modelNodes {
		coeff, ok := coefficients[modelID]
		if !ok {
			continue
		}
		rawModel := int64(0)
		for _, node := range nodes {
			if node != nil {
				rawModel += node.PocWeight
			}
		}
		total += coeff.MulInt64(rawModel).TruncateInt64()
	}
	return total
}

func confirmationCoefficients(scales []*ConfirmationWeightScale) map[string]mathsdk.LegacyDec {
	coefficients := make(map[string]mathsdk.LegacyDec, len(scales))
	for _, scale := range scales {
		if scale == nil || scale.ModelId == "" {
			continue
		}
		coefficients[scale.ModelId] = confirmationScaleFactor(scale)
	}
	return coefficients
}

func confirmationScaleFactor(scale *ConfirmationWeightScale) mathsdk.LegacyDec {
	if scale == nil || scale.WeightScaleFactor == nil ||
		(scale.WeightScaleFactor.Value == 0 && scale.WeightScaleFactor.Exponent == 0) {
		return mathsdk.LegacyOneDec()
	}
	dec, err := scale.WeightScaleFactor.ToLegacyDec()
	if err != nil {
		return mathsdk.LegacyOneDec()
	}
	return dec
}
