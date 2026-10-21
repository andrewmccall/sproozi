package remote

import (
	"errors"
	"slices"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/andrewmccall/sproozi/internal/session/profile"
)

const apiAudience = "https://kubernetes.default.svc"

func TestReviewerAuthorizesByTokenReviewAndRBAC(t *testing.T) {
	const cli = "system:serviceaccount:team-a:sproozi-cli"
	for _, tc := range []struct {
		name, token string
		want        func(error) bool
	}{
		{"allowed ServiceAccount", "cli-token", func(err error) bool { return err == nil }},
		{"unknown token", "stranger", func(err error) bool { return errors.Is(err, profile.ErrUnauthenticated) }},
		{"authenticated without the role", "other-token", func(err error) bool {
			var r *profile.Rejection
			return errors.As(err, &r) && r.Code == profile.CodeDenied
		}},
		{"empty token", "", func(err error) bool { return errors.Is(err, profile.ErrUnauthenticated) }},
		// A developer's own credential, valid for the API server, is never
		// accepted, even for an identity RBAC allows.
		{"API server token of an allowed identity", "api-token", func(err error) bool {
			return errors.Is(err, profile.ErrUnauthenticated)
		}},
		// A review answered without audiences did not check the audience.
		{"audience not checked", "unaware-token", func(err error) bool {
			return errors.Is(err, profile.ErrUnauthenticated)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := fake.NewClientset()
			kube.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
				review := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
				// Model an audience-aware API server: a review with no
				// audiences checks the API server's own.
				requested := review.Spec.Audiences
				if len(requested) == 0 {
					requested = []string{apiAudience}
				}
				user := authenticationv1.UserInfo{Username: cli, Groups: []string{"system:serviceaccounts"},
					Extra: map[string]authenticationv1.ExtraValue{"k": {"v"}}}
				var audiences []string
				switch review.Spec.Token {
				case "cli-token":
					audiences = []string{TokenAudience}
				case "api-token":
					audiences = []string{apiAudience}
				case "other-token":
					audiences, user = []string{TokenAudience}, authenticationv1.UserInfo{Username: "someone"}
				case "unaware-token":
					review.Status = authenticationv1.TokenReviewStatus{Authenticated: true, User: user}
					return true, review, nil
				}
				var granted []string
				for _, audience := range audiences {
					if slices.Contains(requested, audience) {
						granted = append(granted, audience)
					}
				}
				if len(granted) > 0 {
					review.Status = authenticationv1.TokenReviewStatus{Authenticated: true, User: user, Audiences: granted}
				}
				return true, review, nil
			})
			kube.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
				review := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
				r := review.Spec.ResourceAttributes
				review.Status.Allowed = review.Spec.User == cli && review.Spec.Extra["k"][0] == "v" &&
					r.Namespace == "team-a" && r.Verb == PermissionVerb && r.Group == PermissionGroup &&
					r.Resource == PermissionResource && r.Name == testExecutor
				return true, review, nil
			})
			reviewer, err := NewReviewer(kube, "team-a", testExecutor)
			if err != nil {
				t.Fatal(err)
			}
			if err = reviewer.Authorize(t.Context(), tc.token); !tc.want(err) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestReviewerReportsAnUndecidedReviewAsUnavailable(t *testing.T) {
	kube := fake.NewClientset()
	kube.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api server unreachable")
	})
	reviewer, err := NewReviewer(kube, "team-a", testExecutor)
	if err != nil {
		t.Fatal(err)
	}
	err = reviewer.Authorize(t.Context(), "cli-token")
	if status, code := profile.AuthorizationStatus(err); status != 503 || code != profile.CodeUnavailable {
		t.Fatalf("got %v → %d %s", err, status, code)
	}
}
