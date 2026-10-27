import axios from 'axios';

// Create a generic Axios instance for REST calls
export const apiClient = axios.create({
  baseURL: '/api', // Mounts to the Go Node Backend
  timeout: 10000,
});

// Request Interceptor to attach JWT token
apiClient.interceptors.request.use(
  (config) => {
    const token = localStorage.getItem('magnolia_token');
    if (token) {
      config.headers.Authorization = `Bearer ${token}`;
    }
    return config;
  },
  (error) => Promise.reject(error)
);

// Response Interceptor to handle global errors
apiClient.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error.response?.status === 401) {
      localStorage.removeItem('magnolia_token');
      window.location.href = '/auth';
    }
    return Promise.reject(error);
  }
);
