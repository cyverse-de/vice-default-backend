de-default-backend
==================

Provides a default backend handler for the Kubernetes Ingress that handles
routing for VICE apps. This backend decides whether to redirect requests to the
loading page service, the landing page service, or to a 404 page depending on
whether the URL is valid or not.
