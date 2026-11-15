import { createContext, useContext } from "react";
import type { User } from "@/lib/api";

interface AuthContextType {
  user: User | null;
  loading: boolean;
  logout: () => Promise<void>;
  /** Read /auth/me again — after a change to something it carries, such as
   *  the display time zone. */
  refresh: () => Promise<void>;
}

export const AuthContext = createContext<AuthContextType>({
  user: null,
  loading: true,
  logout: async () => {},
  refresh: async () => {},
});

export function useAuth() {
  return useContext(AuthContext);
}
