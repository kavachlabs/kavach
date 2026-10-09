use std::collections::HashMap;

use crate::{GatewayError, Scope};

/// A gateway's connection: sends a request, returns the response bytes as the
/// connection received them (SPEC.md section 4.7).
pub type Connection = Box<dyn FnMut(&[u8]) -> Result<Vec<u8>, GatewayError>>;

type Fallback = Box<dyn FnMut(&str, &[u8]) -> Result<Vec<u8>, GatewayError>>;

/// The gateways a handler may query: name to connection and [`Scope`].
///
/// Names MUST be stable across builds, and so must scopes (SPEC.md section 4.7).
/// A recorder calls the connection; a host only asks for the scope.
#[derive(Default)]
pub struct Gateways {
    named: HashMap<String, (Scope, Connection)>,
    fallback: Option<(Scope, Fallback)>,
}

impl Gateways {
    pub fn new() -> Gateways {
        Gateways::default()
    }

    /// Registers gateway `name`.
    pub fn register(
        mut self,
        name: impl Into<String>,
        scope: Scope,
        connection: impl FnMut(&[u8]) -> Result<Vec<u8>, GatewayError> + 'static,
    ) -> Gateways {
        self.named
            .insert(name.into(), (scope, Box::new(connection)));
        self
    }

    /// Serves every gateway name that is not registered. Used by test programs
    /// such as the conformance handler, which queries arbitrary names.
    pub fn fallback(
        mut self,
        scope: Scope,
        connection: impl FnMut(&str, &[u8]) -> Result<Vec<u8>, GatewayError> + 'static,
    ) -> Gateways {
        self.fallback = Some((scope, Box::new(connection)));
        self
    }

    /// Only the scope of every unregistered name, with no connection.
    pub fn default_scope(self, scope: Scope) -> Gateways {
        self.fallback(scope, |name, _| {
            Err(GatewayError::new(format!(
                "gateway {name:?} has no connection"
            )))
        })
    }

    /// The declared scope of `name`; [`Scope::Remote`] if it is unknown.
    pub(crate) fn scope(&self, name: &str) -> Scope {
        match self.named.get(name) {
            Some((s, _)) => *s,
            None => self.fallback.as_ref().map_or(Scope::Remote, |(s, _)| *s),
        }
    }

    /// Calls the connection of `name`.
    pub(crate) fn call(&mut self, name: &str, request: &[u8]) -> Result<Vec<u8>, GatewayError> {
        if let Some((_, conn)) = self.named.get_mut(name) {
            return conn(request);
        }
        match self.fallback.as_mut() {
            Some((_, conn)) => conn(name, request),
            None => Err(GatewayError::new(format!("unknown gateway {name:?}"))),
        }
    }
}
