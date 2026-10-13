package work

import (
	"errors"
	"maps"

	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
)

func (o *Owner) access(r record) error {
	if !project.Member(o.config.Projects, r.Association.Project, o.config.Principal) {
		return errors.New("work: project is unavailable to this installation principal")
	}
	return nil
}

func (o *Owner) resolve(q Submit) (project.Association, Repository, map[string]session.Allowance, error) {
	association := project.Association{Project: q.Project, Template: o.config.TemplateName, Repository: q.Repository}
	allowances := maps.Clone(o.config.Allowances)
	if q.Project != "" {
		if !project.Member(o.config.Projects, q.Project, o.config.Principal) {
			return association, Repository{}, nil, errors.New("work: project is unavailable to this installation principal")
		}
		d := o.config.Projects[q.Project]
		if q.Repository != "" && q.Repository != d.Workspace {
			return association, Repository{}, nil, errors.New("work: repository conflicts with the project's configured workspace")
		}
		association.Repository = d.Workspace
		var err error
		allowances, err = project.Narrow(d.Capabilities, o.config.Allowances)
		if err != nil {
			return association, Repository{}, nil, err
		}
	}
	r, ok := o.config.Repositories[association.Repository]
	if !ok && (q.Project == "" || association.Repository != "") {
		return association, Repository{}, nil, errors.New("work: repository not approved")
	}
	association.RepositoryReference = r.Reference
	if association.RepositoryReference == "" {
		association.RepositoryReference = r.URL
	}
	if err := project.ValidateReference(association.RepositoryReference); err != nil {
		return association, Repository{}, nil, err
	}
	return association, r, allowances, nil
}
