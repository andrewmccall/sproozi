package remote

import (
	"context"
	"errors"
	"slices"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// Reviewer is the remote executor's side: it authorizes the home principal by
// the caller's Kubernetes identity. A TokenReview authenticates the bearer
// token for TokenAudience only, so a token the API server accepts, such as a
// developer's own credential, is refused; a SubjectAccessReview asks whether that identity may "use"
// remoteexecutors.sproozi.com with the executor's placement name in its
// namespace. Removing the RoleBinding revokes access on the next request.
type Reviewer struct {
	kube      kubernetes.Interface
	namespace string
	name      string
}

// NewReviewer authorizes callers of the executor named name in namespace. The
// executor's own identity needs to create TokenReviews and SubjectAccessReviews
// (the system:auth-delegator ClusterRole).
func NewReviewer(kube kubernetes.Interface, namespace, name string) (*Reviewer, error) {
	if kube == nil || namespaceProblem(namespace) != "" || name == "" {
		return nil, errors.New("remote: reviewer needs a client, a namespace and the executor's placement name")
	}
	return &Reviewer{kube: kube, namespace: namespace, name: name}, nil
}

// Authorize implements profile.Authorizer.
func (r *Reviewer) Authorize(ctx context.Context, token string) error {
	if token == "" {
		return profile.ErrUnauthenticated
	}
	review, err := r.kube.AuthenticationV1().TokenReviews().Create(ctx,
		&authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{
			Token: token, Audiences: []string{TokenAudience},
		}}, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	// An audience-aware API server returns the audiences the token was
	// accepted for; an empty or different list means the audience was not
	// checked, so the token may be one the API server itself accepts.
	if !review.Status.Authenticated || !slices.Contains(review.Status.Audiences, TokenAudience) {
		return profile.ErrUnauthenticated
	}
	user := review.Status.User
	extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
	for key, values := range user.Extra {
		extra[key] = authorizationv1.ExtraValue(values)
	}
	access, err := r.kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User: user.Username, UID: user.UID, Groups: user.Groups, Extra: extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: r.namespace, Verb: PermissionVerb, Group: PermissionGroup,
				Resource: PermissionResource, Name: r.name,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	if !access.Status.Allowed {
		return &profile.Rejection{Code: profile.CodeDenied}
	}
	return nil
}

var _ profile.Authorizer = (*Reviewer)(nil)
