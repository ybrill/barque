package repobuilder

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/evergreen-ci/bond"
	"github.com/evergreen-ci/utility"
	"github.com/mongodb/amboy/dependency"
	"github.com/mongodb/grip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type RepoJobSuite struct {
	j       *repoBuilderJob
	require *require.Assertions
	suite.Suite
}

func TestRepoJobSuite(t *testing.T) {
	suite.Run(t, new(RepoJobSuite))
}

func (s *RepoJobSuite) SetupSuite() {
	s.require = s.Require()
}

func (s *RepoJobSuite) SetupTest() {
	s.j = buildRepoJob()
	s.require.NotNil(s.j)
}

func (s *RepoJobSuite) TearDownTest() {
	for _, path := range s.j.workingDirs {
		grip.Error(os.RemoveAll(filepath.Join(s.j.Conf.WorkSpace, path)))
	}
}

func (s *RepoJobSuite) TestDependencyAccessorIsCorrect() {
	s.Equal("always", s.j.Dependency().Type().Name)
}

func (s *RepoJobSuite) TestSetDependencyAcceptsDifferentAlwaysRunInstances() {
	originalDep := s.j.Dependency()
	newDep := dependency.NewAlways()
	s.True(originalDep != newDep)

	s.j.SetDependency(newDep)
	s.True(originalDep != s.j.Dependency())
	s.Exactly(newDep, s.j.Dependency())
}

func TestProcessPackages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	j := buildRepoJob()
	var err error
	j.release, err = bond.CreateMongoDBVersion("4.4.5")
	require.NoError(t, err)
	j.client = utility.GetDefaultHTTPRetryableClient()
	j.Distro = &RepositoryDefinition{Name: "test"}
	defer func() { utility.PutHTTPClient(j.client) }()
	j.PackagePaths = []string{"https://mciuploads.s3.amazonaws.com/mongo-release/enterprise-rhel-80-64-bit/d18913b155d170b760eede8c457f6d9ed2969aab/artifacts/mongo_release_enterprise_rhel_80_64_bit_d18913b155d170b760eede8c457f6d9ed2969aab_21_04_01_22_38_07-packages.tgz"}
	j.tmpdir, err = ioutil.TempDir("", "test-process-packages")
	require.NoError(t, err)

	assert.NoError(t, j.processPackages(ctx))
	assert.Len(t, j.PackagePaths, 7)
}

func TestSetNotaryKey(t *testing.T) {
	cases := []struct {
		mongoVersion  string
		wantNotaryKey string
	}{
		{mongoVersion: "5.0.0-rc0-29-gafea6bf", wantNotaryKey: "server-5.0"},
		{mongoVersion: "3.4.0-rc24", wantNotaryKey: "server-3.4"},
		{mongoVersion: "4.2.0", wantNotaryKey: "server-4.2"},
		{mongoVersion: "5.1.2-alpha", wantNotaryKey: "server-5.0"},
		{mongoVersion: "3.7.1", wantNotaryKey: "server-3.8"},
	}

	for _, tc := range cases {
		t.Run(tc.mongoVersion, func(t *testing.T) {
			j := buildRepoJob()

			release, err := bond.CreateMongoDBVersion(tc.mongoVersion)
			if err != nil {
				t.Errorf("%q: could not make mongo db version: %v", tc.mongoVersion, err)
			}

			j.release = release
			// TODO: implement test cases for DEB as well
			distro := &RepositoryDefinition{
				Type: RPM,
			}
			j.Distro = distro

			setNotaryKey(j)
			if j.NotaryKey != tc.wantNotaryKey {
				t.Errorf("%q. setNotaryKey() = %v, want %v", tc.mongoVersion, j.NotaryKey, tc.wantNotaryKey)
			}

		})
	}

}

func TestGetPackageLocation(t *testing.T) {
	for _, test := range []struct {
		name             string
		version          string
		expectedLocation string
	}{
		{
			name:             "LegacyReleaseCandidate",
			version:          "4.2.5-rc1",
			expectedLocation: "testing",
		},
		{
			name:             "LegacyDevelopmentBuild",
			version:          "4.1.5-pre-",
			expectedLocation: "development",
		},
		{
			name:             "LegacyDevelopmentSeries",
			version:          "4.1.5",
			expectedLocation: "4.1",
		},
		{
			name:             "LegacyProductionSeries",
			version:          "4.2.5",
			expectedLocation: "4.2",
		},
		{
			name:             "NewReleaseCandidate",
			version:          "5.3.5-rc1",
			expectedLocation: "testing",
		},
		{
			name:             "NewDevelopmentReleaseLTS",
			version:          "5.0.5-alpha1",
			expectedLocation: "development",
		},
		{
			name:             "NewDevelopmentReleaseQuarterly",
			version:          "5.2.5-alpha1",
			expectedLocation: "development",
		},
		{
			name:             "NewQuarterly",
			version:          "5.3.5",
			expectedLocation: "development",
		},
		{
			name:             "NewLTS",
			version:          "5.0.5",
			expectedLocation: "5.0",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var err error
			j := buildRepoJob()
			j.release, err = bond.CreateMongoDBVersion(test.version)
			require.NoError(t, err)
			assert.Equal(t, test.expectedLocation, j.getPackageLocation())
		})
	}
}
