// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pathway

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/pkg/errors"
	"github.com/google/simhospital/pkg/sample"
)

const (
	// maxSignificantDigits is the maximum number of decimal digits allowed in the
	// 'percentage_of_patients' field in pathways.
	maxSignificantDigits = 3
	// defaultPercentage is the default 'percentage_of_patients' for pathways that don't specify it.
	defaultPercentage = 1
)

// DistributionManager manages a given distribution of pathways.
type DistributionManager struct {
	// pathways is a map of pathways indexed by their name.
	pathways map[string]Pathway
	// distribution is the distribution of pathways.
	distribution *sample.DiscreteDistribution
	// pathwayNames is a list of the names of all pathways this manager contains.
	pathwayNames []string
}

// getPathway returns the pathway for the specified name.
// If the pathway does not exist, then it returns an error.
func (m DistributionManager) getPathway(pathwayName string) (Pathway, error) {
	pathway, ok := m.pathways[pathwayName]
	if !ok {
		return pathway, fmt.Errorf("pathwayName %s does not exist within manager.pathways", pathwayName)
	}
	return pathway.Runnable()
}

// GetPathway gets the pathway with the given name.
// If the name provided is not valid, it returns an error.
func (m DistributionManager) GetPathway(pathwayName string) (*Pathway, error) {
	pathway, err := m.getPathway(pathwayName)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get pathway with name %s", pathwayName)
	}
	return &pathway, nil
}

// NextPathway returns the next pathway to run.
// This is chosen based on the expected frequency for each pathway in the original pathway list.
// If there are no eligible pathways (e.g. all pathways are disabled), NextPathway returns an error.
func (m DistributionManager) NextPathway() (*Pathway, error) {
	r := m.distribution.Random()
	if r == nil {
		return nil, errors.New("all pathways are disabled")
	}
	nextPathwayName := r.(string)
	return m.GetPathway(nextPathwayName)
}

// AllPathwayNames returns all the pathway names in this manager.
func (m DistributionManager) AllPathwayNames() []string {
	return m.pathwayNames
}

// NewDistributionManager creates a new DistributionManager with the given pathway map.
// All pathways are initialised.
// If includeStr contains any elements, then only pathways that match any regex in includeStr are eligible
// to be returned by NextPathway.
// Pathways that match any regex in excludeStr are never returned by NextPathway.
func NewDistributionManager(pathways map[string]Pathway, includeStr []string, excludeStr []string) (DistributionManager, error) {
	include, err := toRegexps(includeStr)
	if err != nil {
		return DistributionManager{}, errors.Wrapf(err, "Failed to convert %v to regexps", include)
	}
	exclude, err := toRegexps(excludeStr)
	if err != nil {
		return DistributionManager{}, errors.Wrapf(err, "Failed to convert %v to regexps", exclude)
	}

	m := DistributionManager{
		pathways: map[string]Pathway{},
	}
	for k, v := range pathways {
		v.Init(k)
		m.pathways[k] = v
		m.pathwayNames = append(m.pathwayNames, k)
	}

	distr, percentages := calculateDistribution(m.pathways, include, exclude)
	m.distribution = &sample.DiscreteDistribution{WeightedValues: distr}
	printPathwayStats(m.pathways, percentages)
	return m, nil
}

func calculateDistribution(pathways map[string]Pathway, include []*regexp.Regexp, exclude []*regexp.Regexp) ([]sample.WeightedValue, map[string]float64) {
	percentages := map[string]float64{}
	var weighted []sample.WeightedValue
	accPercentage := 0.0
	// We'll later share the remaining percentage budget among the pathways without an explicit one.
	var noPercentage []string
	for k, v := range pathways {
		switch {
		case len(include) > 0 && !matches(k, include) || matches(k, exclude):
			log.WithField("pathway_name", k).Debug("Pathway disabled")
			percentages[k] = 0
		case v.Percentage == nil:
			noPercentage = append(noPercentage, k)
		case *v.Percentage > 0:
			accPercentage, weighted = addPercentage(v.Percentage.Float(), accPercentage, k, weighted)
			percentages[k] = v.Percentage.Float()
		}
	}

	// Share the remaining percentage_of_patients budget among the pathways without an explicit one.
	optimal := 100.0
	if len(noPercentage) > 0 {
		remaining := optimal - accPercentage
		perPathway := calculateBudgetPerPathway(remaining, len(noPercentage))
		log.Infof("Setting pathway frequency %v%% for %d pathways without explicit percentage_of_patients: %v", perPathway, len(noPercentage), noPercentage)
		for _, k := range noPercentage {
			accPercentage, weighted = addPercentage(perPathway, accPercentage, k, weighted)
			percentages[k] = perPathway
		}
	}
	log.Infof("Accumulated percentage_of_patients is %.3f. The closer to 100, "+
		"the closer the actual distribution will be to the pathways' percentage_of_patients", accPercentage)
	return weighted, percentages
}

func addPercentage(f, acc float64, name string, weighted []sample.WeightedValue) (float64, []sample.WeightedValue) {
	// Our distribution expects an integer.
	frequency := f * math.Pow(10.0, float64(maxSignificantDigits))
	weighted = append(weighted, sample.WeightedValue{Value: name, Frequency: uint(frequency)})
	return acc + f, weighted
}

// calculateBudgetPerPathway splits the remaining percentage budget among n pathways and returns
// the percentage that should be set per pathway. If there's not enough budget to set relevant
// percentages, it returns a default percentage.
func calculateBudgetPerPathway(remaining float64, n int) float64 {
	perPathway := remaining / float64(n)
	if perPathway <= 0 {
		log.Warningf("Cannot split remaining percentage budget %.3f among %d pathways, invalid percentage per pathway: %v. Setting default percentage: %v%%",
			remaining, n, perPathway, defaultPercentage)
		perPathway = defaultPercentage
	}
	return round(perPathway)
}

func printPathwayStats(pathways map[string]Pathway, percentage map[string]float64) {
	var names []string
	for name := range pathways {
		names = append(names, name)
	}

	sort.Strings(names)

	log.Infof("Loaded %d pathways:", len(pathways))
	for _, name := range names {
		suffix := ""
		if percentage[name] == 0 {
			suffix = "(percentage set to 0; this pathway will not be run)"
		}
		log.Infof(" - %s, percentage=%v %s", name, percentage[name], suffix)
	}
}

func toRegexps(strings []string) ([]*regexp.Regexp, error) {
	regexps := make([]*regexp.Regexp, len(strings))
	for i, s := range strings {
		r, err := regexp.Compile(wrapRegexp(s))
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to compile regexp %s", s)
		}
		regexps[i] = r
	}
	return regexps, nil
}

func matches(s string, regex []*regexp.Regexp) bool {
	for _, r := range regex {
		if r.MatchString(s) {
			return true
		}
	}
	return false
}

func wrapRegexp(s string) string {
	if !strings.HasPrefix(s, "^") {
		s = fmt.Sprintf("^%s", s)
	}
	if !strings.HasSuffix(s, "$") {
		s = fmt.Sprintf("%s$", s)
	}
	return s
}
