import { RecordCard } from "./components/RecordCard";
import { useStatus } from "./useStatus";

export default function App() {
  const { status, error, loading } = useStatus();

  return <RecordCard status={status} error={error} loading={loading} />;
}
