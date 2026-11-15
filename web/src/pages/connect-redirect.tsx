import { Navigate, useLocation } from "react-router-dom";
import { connectRedirectTarget } from "@/lib/connect-redirect";

/** rsconnect's post-deploy address; the decision is `connectRedirectTarget`. */
export function ConnectRedirectPage() {
  const { hash } = useLocation();
  return <Navigate to={connectRedirectTarget(hash)} replace />;
}
